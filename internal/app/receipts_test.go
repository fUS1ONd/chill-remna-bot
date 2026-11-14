package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"remnabot/internal/config"
	"remnabot/internal/model"
	"remnabot/internal/moynalog"
	"remnabot/internal/remnawave"
	"remnabot/internal/yookassa"
)

// fakeFNS — кабинет «Мой налог». answer решает исход n-го обращения (с 1);
// по умолчанию ФНС принимает доход.
type fakeFNS struct {
	mu     sync.Mutex
	got    []moynalog.Income
	answer func(n int) (string, error)
	// before вызывается до ответа: тесты смотрят, что лежит в базе в момент
	// обращения к ФНС.
	before func(in moynalog.Income)
}

func (f *fakeFNS) CreateIncome(_ context.Context, in moynalog.Income) (string, error) {
	f.mu.Lock()
	f.got = append(f.got, in)
	n := len(f.got)
	answer, before := f.answer, f.before
	f.mu.Unlock()
	if before != nil {
		before(in)
	}
	if answer != nil {
		return answer(n)
	}
	return "uuid-" + itoa(n), nil
}

func (f *fakeFNS) calls() []moynalog.Income {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]moynalog.Income(nil), f.got...)
}

// Ответ ФНС потерян после приёма: доход у ФНС есть, а uuid до нас не дошёл.
func fnsLost(int) (string, error) {
	return "", &moynalog.Error{Kind: moynalog.KindUnknown, Msg: "связь оборвалась"}
}

func fnsRejected(int) (string, error) {
	return "", &moynalog.Error{Kind: moynalog.KindRejected, Msg: "status 400: validation.failed"}
}

func fnsDown(int) (string, error) {
	return "", &moynalog.Error{Kind: moynalog.KindRetry, Msg: "status 503"}
}

func receiptApp(t *testing.T) (*App, *fakeMsg, *fakeStore, *fakeFNS) {
	t.Helper()
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/by-telegram-id/") {
			_, _ = w.Write([]byte(`{"response":[{"uuid":"u1","tag":"CHILLBOT","username":"tg_555","subscriptionUrl":"https://sub/x","expireAt":"2030-01-01T00:00:00Z"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"uuid":"u1","subscriptionUrl":"https://sub/x","expireAt":"2030-01-01T00:00:00Z"}}`))
	}))
	t.Cleanup(panel.Close)

	fm := &fakeMsg{}
	fs := &fakeStore{}
	fns := &fakeFNS{}
	a := &App{
		cfg:   &config.Config{AdminID: 100, DataDir: t.TempDir()},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		msg:   fm,
		store: fs,
		ui:    map[int64]*uiState{},
	}
	a.botCfg = &model.BotConfig{Installed: true, Language: "ru", Pricing: model.Pricing{Currency: "₽", Base: map[int]string{1: "150"}}}
	a.botCfg.NormalizePricing()
	a.botCfg.MoyNalog = model.MoyNalogConfig{Enabled: true, Login: "79990000000", Password: "secret", ServiceName: "VPN"}
	a.mnNew = func(login, pass string) moyNalogAPI { return fns }
	a.panel = remnawave.New(model.PanelConfig{Mode: model.ModeRemote, BaseURL: panel.URL, APIToken: "t"})
	_ = fs.UpsertUser(context.Background(), 555)
	return a, fm, fs, fns
}

// Каждая оплата через ЮKassa — тариф и пополнение — получает чек с uuid ФНС.
func TestReceipt_YooKassaPaymentsGetReceipt(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()

	if _, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-buy-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.finalizeTopUp(ctx, 555, 50050, model.PayMethodYooKassa, "500.50 ₽", "yk-top-1"); err != nil {
		t.Fatal(err)
	}
	a.moneyWG.Wait()

	calls := fns.calls()
	if len(calls) != 2 {
		t.Fatalf("ожидалось два чека, ушло %d", len(calls))
	}
	for _, c := range calls {
		if !strings.HasPrefix(c.Name, "VPN (") || c.OperationTime.IsZero() {
			t.Fatalf("чек без метки или даты: %+v", c)
		}
	}
	buy, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-buy-1")
	top, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-top-1")
	if buy == nil || buy.State != model.ReceiptCreated || buy.UUID == "" || buy.AmountKopecks != 15000 {
		t.Fatalf("чек за тариф: %+v", buy)
	}
	if top == nil || top.State != model.ReceiptCreated || top.UUID == "" || top.AmountKopecks != 50050 {
		t.Fatalf("чек за пополнение: %+v", top)
	}
	if buy.Name == top.Name || buy.Marker == "" || !strings.Contains(buy.Name, buy.Marker) {
		t.Fatalf("метка должна быть своя у каждого чека и стоять в наименовании: %q / %q", buy.Name, top.Name)
	}
}

// Право на чек столбится строкой в базе ДО обращения к ФНС: иначе потерянный
// ответ и повторная финализация не оставили бы следа, по которому искать чек.
func TestReceipt_ClaimedBeforeFNS(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	var seen *model.Receipt
	fns.before = func(moynalog.Income) {
		seen, _ = fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-order")
	}
	if err := a.finalizeTopUp(ctx, 555, 10000, model.PayMethodYooKassa, "100 ₽", "yk-order"); err != nil {
		t.Fatal(err)
	}
	a.moneyWG.Wait()
	if seen == nil || seen.State != model.ReceiptPending {
		t.Fatalf("в момент обращения к ФНС строка чека должна уже лежать в pending, got %+v", seen)
	}
}

// Повторная финализация того же платежа второй чек не пробивает — даже когда
// запись платежа в первый раз сорвалась и барьер по ext_id не сработал.
func TestReceipt_RepeatedFinalizationNoSecondReceipt(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()

	fs.addPaymentErr = errors.New("database is locked")
	_, _, _ = a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-dup", nil)
	fs.addPaymentErr = nil
	_, _, _ = a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-dup", nil)
	_, _, _ = a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-dup", nil)

	if err := a.finalizeTopUp(ctx, 555, 10000, model.PayMethodYooKassa, "100 ₽", "yk-top-dup"); err != nil {
		t.Fatal(err)
	}
	if err := a.finalizeTopUp(ctx, 555, 10000, model.PayMethodYooKassa, "100 ₽", "yk-top-dup"); err != nil {
		t.Fatal(err)
	}
	a.moneyWG.Wait()

	if n := len(fns.calls()); n != 2 {
		t.Fatalf("на два платежа должно уйти два чека, ушло %d", n)
	}
}

// Исход обращения к ФНС записывается в строку чека.
func TestReceipt_OutcomesRecorded(t *testing.T) {
	cases := []struct {
		name   string
		answer func(int) (string, error)
		state  string
		uuid   bool
	}{
		{"ответил", nil, model.ReceiptCreated, true},
		{"потерял ответ после приёма", fnsLost, model.ReceiptUnknown, false},
		{"отказал", fnsRejected, model.ReceiptRejected, false},
		{"недоступна", fnsDown, model.ReceiptPending, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, fs, fns := receiptApp(t)
			ctx := context.Background()
			fns.answer = tc.answer
			if err := a.finalizeTopUp(ctx, 555, 10000, model.PayMethodYooKassa, "100 ₽", "yk-out"); err != nil {
				t.Fatal(err)
			}
			a.moneyWG.Wait()
			r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-out")
			if r == nil || r.State != tc.state || (r.UUID != "") != tc.uuid || r.Attempts != 1 {
				t.Fatalf("строка чека: %+v", r)
			}
			if tc.state != model.ReceiptCreated && r.LastError == "" {
				t.Fatalf("причина неудачи не записана: %+v", r)
			}
		})
	}
}

// Оплата с баланса и деньги через других провайдеров чек не пробивают.
func TestReceipt_NotForBalanceAndOtherProviders(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()

	if err := a.finalizeTopUp(ctx, 555, 50000, model.PayMethodCryptoBot, "5 USDT", "cb-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodPlatega, "150 ₽", "pl-1", nil); err != nil {
		t.Fatal(err)
	}
	a.onBuyPlan(ctx, 555, "1")
	a.payFromBalance(ctx, 555)
	if u, _ := fs.GetUser(ctx, 555); u == nil || u.Balance != 50000-15000 {
		t.Fatalf("оплата с баланса не прошла: %+v", u)
	}
	a.moneyWG.Wait()

	if n := len(fns.calls()); n != 0 {
		t.Fatalf("чеков быть не должно, ушло %d", n)
	}
	if len(fs.receipts) != 0 {
		t.Fatalf("строк чеков быть не должно: %v", fs.receipts)
	}
}

// Автосписание — тоже деньги через ЮKassa и тоже получает чек.
func TestReceipt_AutoChargeGetsReceipt(t *testing.T) {
	yk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"yk-auto-1","status":"succeeded","paid":true,"amount":{"value":"150.00","currency":"RUB"},"metadata":{"months":"1","telegram_id":"555"}}`))
	}))
	t.Cleanup(yk.Close)
	oldBase := yookassa.BaseURL
	yookassa.BaseURL = yk.URL
	t.Cleanup(func() { yookassa.BaseURL = oldBase })

	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	a.botCfg.YooKassa = model.YooKassaConfig{Enabled: true, ShopID: "shop", SecretKey: "sec", AutoPay: true, AutoPayDays: 1}
	now := time.Now().UTC()
	exp := now.Add(12 * time.Hour)
	_ = fs.SetSubExpiry(ctx, 555, exp.Format(time.RFC3339), "paid")
	ap := &model.AutoPay{TelegramID: 555, Method: model.PayMethodYooKassa, MethodID: "pm", Months: 1, Enabled: true}
	_ = fs.SetAutoPay(ctx, ap)

	if reason := a.chargeAutoPay(ctx, ap, now, exp); reason != "" {
		t.Fatalf("автосписание не прошло: %s", reason)
	}
	a.moneyWG.Wait()

	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-auto-1")
	if len(fns.calls()) != 1 || r == nil || r.State != model.ReceiptCreated || r.AmountKopecks != 15000 {
		t.Fatalf("чек автосписания: вызовов %d, строка %+v", len(fns.calls()), r)
	}
}

// ykPayments — ЮKassa, отвечающая на запрос платежа телом из pays по его id
// (и на создание автосписания — платежом "yk-auto").
func ykPayments(t *testing.T, pays map[string]string) {
	t.Helper()
	yk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if r.Method == http.MethodPost {
			id = "yk-auto"
		}
		body, ok := pays[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(yk.Close)
	oldBase := yookassa.BaseURL
	yookassa.BaseURL = yk.URL
	t.Cleanup(func() { yookassa.BaseURL = oldBase })
}

// ykSucceeded — тело подтверждённого платежа ЮKassa; captured="" — без
// captured_at.
func ykSucceeded(id, captured string) string {
	c := ""
	if captured != "" {
		c = `,"captured_at":"` + captured + `"`
	}
	return `{"id":"` + id + `","status":"succeeded","paid":true,"amount":{"value":"150.00","currency":"RUB"},` +
		`"metadata":{"months":"1","telegram_id":"555"}` + c + `}`
}

func paymentByExt(fs *fakeStore, extID string) *model.Payment {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, p := range fs.pays {
		if p.ExtID == extID {
			cp := *p
			return &cp
		}
	}
	return nil
}

// Оплата, подхваченная сверкой уже в следующем месяце, датируется моментом
// списания: 31 октября 23:59 по Москве — октябрьский доход, хотя бот узнал о
// нём в ноябре.
func TestReceipt_ReconciledLateDatedByCapture(t *testing.T) {
	const captured = "2026-10-31T20:59:30.512Z" // 23:59:30 МСК
	ykPayments(t, map[string]string{
		"yk-late-buy": ykSucceeded("yk-late-buy", captured),
		"yk-late-top": ykSucceeded("yk-late-top", captured),
	})
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	a.botCfg.YooKassa = model.YooKassaConfig{Enabled: true, ShopID: "shop", SecretKey: "sec"}
	created := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	_ = fs.AddPendingInvoice(ctx, &model.PendingInvoice{
		Method: model.PayMethodYooKassa, ExtID: "yk-late-buy", TelegramID: 555, Months: 1, CreatedAt: created,
	})
	_ = fs.AddPendingInvoice(ctx, &model.PendingInvoice{
		Method: model.PayMethodYooKassa, ExtID: "yk-late-top", TelegramID: 555, Purpose: "topup", Kopecks: 15000, CreatedAt: created,
	})

	a.reconcileOnce(ctx)
	a.moneyWG.Wait()

	moscow := time.FixedZone("MSK", 3*60*60)
	for _, ext := range []string{"yk-late-buy", "yk-late-top"} {
		p := paymentByExt(fs, ext)
		if p == nil || p.CapturedAt != "2026-10-31T20:59:30Z" {
			t.Fatalf("%s: запись платежа без момента списания: %+v", ext, p)
		}
		r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, ext)
		if r == nil || r.IncomeAt != "2026-10-31T20:59:30Z" {
			t.Fatalf("%s: чек датирован не списанием: %+v", ext, r)
		}
	}
	calls := fns.calls()
	if len(calls) != 2 {
		t.Fatalf("ожидалось два чека, ушло %d", len(calls))
	}
	for _, c := range calls {
		if m := c.OperationTime.In(moscow); m.Month() != time.October || m.Day() != 31 {
			t.Fatalf("в ФНС ушёл не октябрьский доход: %v", m)
		}
	}
}

// Момент списания из ответа ЮKassa ложится в запись платежа и в чек на каждом
// пути: вебхук, ручная проверка, автосписание.
func TestReceipt_CapturedAtOnEveryYooKassaPath(t *testing.T) {
	const captured = "2026-10-31T20:59:30Z"
	ykPayments(t, map[string]string{
		"yk-hook":  ykSucceeded("yk-hook", captured),
		"yk-check": ykSucceeded("yk-check", captured),
		"yk-auto":  ykSucceeded("yk-auto", captured),
	})
	a, _, fs, _ := receiptApp(t)
	ctx := context.Background()
	a.botCfg.YooKassa = model.YooKassaConfig{Enabled: true, ShopID: "shop", SecretKey: "sec", AutoPay: true, AutoPayDays: 1}

	if _, err := a.HandleYooKassaWebhook(ctx, []byte(`{"event":"payment.succeeded","object":{"id":"yk-hook"}}`)); err != nil {
		t.Fatal(err)
	}
	a.onYKCheck(ctx, 555, "yk-check")
	now := time.Now().UTC()
	exp := now.Add(12 * time.Hour)
	_ = fs.SetSubExpiry(ctx, 555, exp.Format(time.RFC3339), "paid")
	ap := &model.AutoPay{TelegramID: 555, Method: model.PayMethodYooKassa, MethodID: "pm", Months: 1, Enabled: true}
	_ = fs.SetAutoPay(ctx, ap)
	if reason := a.chargeAutoPay(ctx, ap, now, exp); reason != "" {
		t.Fatalf("автосписание не прошло: %s", reason)
	}
	a.moneyWG.Wait()

	for _, ext := range []string{"yk-hook", "yk-check", "yk-auto"} {
		p := paymentByExt(fs, ext)
		r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, ext)
		if p == nil || p.CapturedAt != captured || r == nil || r.IncomeAt != captured {
			t.Fatalf("%s: платёж %+v, чек %+v", ext, p, r)
		}
	}
}

// ЮKassa не прислала captured_at — чек датируется моментом принятия платежа,
// а в записи платежа момент списания пуст.
func TestReceipt_NoCapturedAtDatedByAcceptance(t *testing.T) {
	ykPayments(t, map[string]string{"yk-nocap": ykSucceeded("yk-nocap", "")})
	a, _, fs, _ := receiptApp(t)
	ctx := context.Background()
	a.botCfg.YooKassa = model.YooKassaConfig{Enabled: true, ShopID: "shop", SecretKey: "sec"}

	before := time.Now().UTC().Truncate(time.Second)
	if _, err := a.HandleYooKassaWebhook(ctx, []byte(`{"event":"payment.succeeded","object":{"id":"yk-nocap"}}`)); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	a.moneyWG.Wait()

	if p := paymentByExt(fs, "yk-nocap"); p == nil || p.CapturedAt != "" {
		t.Fatalf("запись платежа: %+v", p)
	}
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-nocap")
	if r == nil {
		t.Fatal("чека нет")
	}
	at, err := time.Parse(time.RFC3339, r.IncomeAt)
	if err != nil || at.Before(before) || at.After(after) {
		t.Fatalf("чек должен датироваться моментом принятия [%v, %v], got %q", before, after, r.IncomeAt)
	}
}
