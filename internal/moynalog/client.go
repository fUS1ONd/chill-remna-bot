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
	"net/url"
	"strconv"
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

// webUserAgent — «браузер», которым бот представляется кабинету ФНС.
// С 17.11.2026 metaDetails.userAgent обязателен: без него ФНС отвечает на
// верный пароль 500 «Внутренняя ошибка», и чек не пробивается. Веб-кабинет
// кладёт сюда navigator.userAgent.
const webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// metaDetails — отпечаток клиента в deviceInfo.
type metaDetails struct {
	UserAgent string `json:"userAgent"`
}

type deviceInfo struct {
	SourceDeviceID string       `json:"sourceDeviceId"`
	SourceType     string       `json:"sourceType"`
	AppVersion     string       `json:"appVersion"`
	MetaDetails    *metaDetails `json:"metaDetails,omitempty"`
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
		Username: c.login,
		Password: c.pass,
		DeviceInfo: deviceInfo{
			SourceDeviceID: "remnabot",
			SourceType:     "WEB",
			AppVersion:     "1.0.0",
			MetaDetails:    &metaDetails{UserAgent: webUserAgent},
		},
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

// timeLayout — формат времени в запросах ФНС: местное время со смещением.
const timeLayout = "2006-01-02T15:04:05-07:00"

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
	case status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		// 5xx, таймаут и «слишком часто» — временные: доход не принят, позже
		// тот же запрос может пройти.
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
		OperationTime:                   op.In(moscow).Format(timeLayout),
		RequestTime:                     now.In(moscow).Format(timeLayout),
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

// IncomeRecord — доход в кабинете ФНС, то есть чек.
type IncomeRecord struct {
	UUID string
	// Names — наименование дохода и наименования услуг в нём.
	Names    []string
	Canceled bool
}

// Matches — стоит ли метка в наименовании чека. Метка ищется в скобках, как
// её ставит бот: шесть символов голыми могли бы встретиться и в самом
// наименовании услуги.
func (r IncomeRecord) Matches(marker string) bool {
	if marker == "" {
		return false
	}
	for _, n := range r.Names {
		if strings.Contains(n, "("+marker+")") {
			return true
		}
	}
	return false
}

// incomesLimit — сколько доходов берётся одной страницей; incomesMaxPages —
// потолок страниц на один список. Окно сверки растёт, пока чек лежит в
// unknown, поэтому страницы обходятся; не дошли до конца — список неполный.
const (
	incomesLimit    = 100
	incomesMaxPages = 10
)

type incomesResponse struct {
	Content []struct {
		ApprovedReceiptUUID string `json:"approvedReceiptUuid"`
		Name                string `json:"name"`
		CancellationInfo    any    `json:"cancellationInfo"`
		Services            []struct {
			Name string `json:"name"`
		} `json:"services"`
	} `json:"content"`
	HasMore bool `json:"hasMore"`
}

// ListIncomes — доходы с from по to. complete=false — список мог обрезаться:
// «чека в нём нет» тогда ничего не значит, а пробить заново по такому
// списку — рискнуть дублем.
func (c *Client) ListIncomes(ctx context.Context, from, to time.Time) (list []IncomeRecord, complete bool, err error) {
	for page := 0; page < incomesMaxPages; page++ {
		ir, err := c.incomesPage(ctx, from, to, page*incomesLimit)
		if err != nil {
			return nil, false, err
		}
		for _, it := range ir.Content {
			r := IncomeRecord{UUID: it.ApprovedReceiptUUID, Names: []string{it.Name}, Canceled: it.CancellationInfo != nil}
			for _, s := range it.Services {
				r.Names = append(r.Names, s.Name)
			}
			list = append(list, r)
		}
		// Полная страница без hasMore — тоже повод смотреть дальше: поле
		// могло не прийти, а обрезанный список за полный не выдаём.
		if !ir.HasMore && len(ir.Content) < incomesLimit {
			return list, true, nil
		}
	}
	return list, false, nil
}

// incomesPage — одна страница списка доходов, с перелогином при отказе токена.
func (c *Client) incomesPage(ctx context.Context, from, to time.Time, offset int) (*incomesResponse, error) {
	q := url.Values{}
	q.Set("from", from.In(moscow).Format(timeLayout))
	q.Set("to", to.In(moscow).Format(timeLayout))
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(incomesLimit))
	q.Set("sortBy", "operation_time:desc")
	path := BaseURL + "/incomes?" + q.Encode()

	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		if err := c.authenticate(ctx); err != nil {
			return nil, &Error{Kind: KindRetry, Msg: "вход в кабинет", Err: err}
		}
	}
	status, raw, err := c.get(ctx, path)
	if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		if err := c.authenticate(ctx); err != nil {
			return nil, &Error{Kind: KindRetry, Msg: "вход в кабинет", Err: err}
		}
		status, raw, err = c.get(ctx, path)
	}
	if err != nil {
		return nil, &Error{Kind: KindRetry, Msg: "список доходов", Err: err}
	}
	if status != http.StatusOK {
		return nil, &Error{Kind: KindRetry, Msg: fmt.Sprintf("список доходов: status %d: %s", status, strings.TrimSpace(string(raw)))}
	}
	var ir incomesResponse
	if err := json.Unmarshal(raw, &ir); err != nil {
		return nil, &Error{Kind: KindRetry, Msg: "нечитаемый список доходов", Err: err}
	}
	return &ir, nil
}

// get — GET с текущим токеном; тело читается целиком, но не больше 1 МиБ.
func (c *Client) get(ctx context.Context, u string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}
