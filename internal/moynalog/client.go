package moynalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	// Гарантирует наличие базы часовых поясов даже в контейнере без tzdata:
	// чек обязан уходить по московскому времени, а не по TZ контейнера (UTC),
	// иначе ночные платежи конца месяца уезжают в предыдущий налоговый период.
	_ "time/tzdata"
)

var BaseURL = "https://lknpd.nalog.ru/api/v1"

// moscow — зона времени чека. ФНС ждёт местное время самозанятого; бот
// ориентирован на РФ, поэтому Москва (при недоступной базе зон — UTC+3).
var moscow = func() *time.Location {
	if loc, err := time.LoadLocation("Europe/Moscow"); err == nil {
		return loc
	}
	return time.FixedZone("MSK", 3*60*60)
}()

type Client struct {
	http  *http.Client
	login string
	pass  string

	mu    sync.Mutex
	token string
	// authMu сериализует сам логин: без него все горутины фискализации после
	// истечения токена устраивали бы залп параллельных парольных логинов.
	authMu sync.Mutex
}

func New(login, pass string) *Client {
	return &Client{
		http:  &http.Client{Timeout: 25 * time.Second},
		login: login,
		pass:  pass,
	}
}

type deviceInfo struct {
	SourceDeviceID string `json:"sourceDeviceId"`
	SourceType     string `json:"sourceType"`
	AppVersion     string `json:"appVersion"`
}

type authRequest struct {
	Username   string     `json:"username"`
	Password   string     `json:"password"`
	DeviceInfo deviceInfo `json:"deviceInfo"`
}

type authResponse struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
}

func (c *Client) authenticate(ctx context.Context) error {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	body, _ := json.Marshal(authRequest{
		Username:   c.login,
		Password:   c.pass,
		DeviceInfo: deviceInfo{SourceDeviceID: "remnabot", SourceType: "WEB", AppVersion: "1.0.0"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/auth/lkfl", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("moynalog auth: status %d: %s", resp.StatusCode, b)
	}
	var ar authResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return err
	}
	if ar.Token == "" {
		return fmt.Errorf("moynalog auth: empty token")
	}
	c.mu.Lock()
	c.token = ar.Token
	c.mu.Unlock()
	return nil
}

type incomeService struct {
	Name     string  `json:"name"`
	Amount   float64 `json:"amount"`
	Quantity int     `json:"quantity"`
}

type incomeClient struct {
	IncomeType string `json:"incomeType"`
}

type incomeRequest struct {
	OperationTime                   string          `json:"operationTime"`
	RequestTime                     string          `json:"requestTime"`
	Services                        []incomeService `json:"services"`
	TotalAmount                     string          `json:"totalAmount"`
	Client                          incomeClient    `json:"client"`
	PaymentType                     string          `json:"paymentType"`
	IgnoreMaxTotalIncomeRestriction bool            `json:"ignoreMaxTotalIncomeRestriction"`
}

type incomeResponse struct {
	ApprovedReceiptUUID string `json:"approvedReceiptUuid"`
	ID                  string `json:"id"`
}

// Kind — что делать с чеком после неудачного обращения к ФНС.
type Kind int

const (
	// KindUnknown — ответ потерян или нечитаем: чек мог создаться. Пробивать
	// заново вслепую нельзя, сначала сверка по метке.
	KindUnknown Kind = iota
	// KindRetry — ФНС чек точно не приняла (не соединились, не вошли в
	// кабинет, 5xx): можно пробить позже тем же запросом.
	KindRetry
	// KindRejected — окончательный отказ 4xx: тот же запрос будет отвергнут
	// так же, повторять бесполезно.
	KindRejected
)

func (k Kind) String() string {
	switch k {
	case KindRetry:
		return "retry"
	case KindRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Error — неудачное обращение к ФНС с классом исхода.
type Error struct {
	Kind Kind
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return "moynalog: " + e.Msg + ": " + e.Err.Error()
	}
	return "moynalog: " + e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf — класс исхода ошибки. Незнакомую ошибку считаем потерянным ответом:
// лишняя сверка по метке дешевле дубля чека.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindUnknown
}

// Income — доход, по которому пробивается чек.
type Income struct {
	Amount float64
	// Name — наименование услуги, как оно попадёт в чек.
	Name string
	// OperationTime — момент дохода; нулевой — момент отправки.
	OperationTime time.Time
}

// CreateIncome пробивает чек и возвращает его uuid. Запрос /income уходит не
// больше одного раза на чек: повтор после потерянного ответа — это второй
// чек. Повторы — забота вызывающего, по классу ошибки (KindOf).
func (c *Client) CreateIncome(ctx context.Context, in Income) (string, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		if err := c.authenticate(ctx); err != nil {
			return "", &Error{Kind: KindRetry, Msg: "вход в кабинет", Err: err}
		}
	}
	id, status, errBody, err := c.createOnce(ctx, in)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Токен отвергнут до приёма дохода — чека нет, перелогин и ровно одна
		// повторная отправка.
		if err := c.authenticate(ctx); err != nil {
			return "", &Error{Kind: KindRetry, Msg: "вход в кабинет", Err: err}
		}
		id, status, errBody, err = c.createOnce(ctx, in)
		if err != nil {
			return "", err
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return "", &Error{Kind: KindRetry, Msg: fmt.Sprintf("кабинет не принял вход: status %d", status)}
		}
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
		if id == "" {
			// Ответ прочитан, а чека в нём нет: создан ли он — неизвестно.
			return "", &Error{Kind: KindUnknown, Msg: fmt.Sprintf("ответ %d без идентификатора чека", status)}
		}
		return id, nil
	case status >= 500:
		return "", &Error{Kind: KindRetry, Msg: fmt.Sprintf("status %d: %s", status, errBody)}
	default:
		// Тело содержит код причины (validation.failed, превышение лимита и т.п.).
		return "", &Error{Kind: KindRejected, Msg: fmt.Sprintf("status %d: %s", status, errBody)}
	}
}

// createOnce — одна отправка /income. Ошибку возвращает уже классифицированной:
// не соединились — KindRetry, оборвалось или не прочиталось после отправки —
// KindUnknown.
func (c *Client) createOnce(ctx context.Context, in Income) (id string, status int, errBody string, err error) {
	now := time.Now()
	op := in.OperationTime
	if op.IsZero() {
		op = now
	}
	body, _ := json.Marshal(incomeRequest{
		OperationTime:                   op.In(moscow).Format("2006-01-02T15:04:05-07:00"),
		RequestTime:                     now.In(moscow).Format("2006-01-02T15:04:05-07:00"),
		Services:                        []incomeService{{Name: in.Name, Amount: in.Amount, Quantity: 1}},
		TotalAmount:                     fmt.Sprintf("%.2f", in.Amount),
		Client:                          incomeClient{IncomeType: "FROM_INDIVIDUAL"},
		PaymentType:                     "CASH",
		IgnoreMaxTotalIncomeRestriction: false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BaseURL+"/income", bytes.NewReader(body))
	if err != nil {
		return "", 0, "", &Error{Kind: KindRetry, Msg: "запрос", Err: err}
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return "", 0, "", &Error{Kind: KindRetry, Msg: "нет соединения", Err: err}
		}
		return "", 0, "", &Error{Kind: KindUnknown, Msg: "связь оборвалась, судьба чека неизвестна", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", resp.StatusCode, "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", 0, "", &Error{Kind: KindUnknown, Msg: "ответ не дочитан, судьба чека неизвестна", Err: err}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", resp.StatusCode, strings.TrimSpace(string(raw)), nil
	}
	var ir incomeResponse
	if err := json.Unmarshal(raw, &ir); err != nil {
		return "", 0, "", &Error{Kind: KindUnknown, Msg: "нечитаемый ответ, судьба чека неизвестна", Err: err}
	}
	id = ir.ApprovedReceiptUUID
	if id == "" {
		id = ir.ID
	}
	return id, resp.StatusCode, "", nil
}
