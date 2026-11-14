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
// по умолчанию ФНС принимает доход. Принятый доход — и с ответом, и с
// потерянным ответом — ложится в список доходов кабинета.
type fakeFNS struct {
	mu     sync.Mutex
	got    []moynalog.Income
	answer func(n int) (string, error)
	// before вызывается до ответа: тесты смотрят, что лежит в базе в момент
	// обращения к ФНС.
	before func(in moynalog.Income)
	// incomes — список доходов кабинета; incomplete — ФНС отдаёт его
	// неполным; listErr — запрос списка не удался.
	incomes    []moynalog.IncomeRecord
	incomplete bool
	listErr    error
	lists      int
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
	uuid, err := "uuid-"+itoa(n), error(nil)
	if answer != nil {
		uuid, err = answer(n)
	}
	if err == nil || moynalog.KindOf(err) == moynalog.KindUnknown {
		// Потерянный ответ — после приёма: доход у ФНС есть.
		f.mu.Lock()
		f.incomes = append(f.incomes, moynalog.IncomeRecord{UUID: "uuid-" + itoa(n), Names: []string{in.Name}})
		f.mu.Unlock()
	}
	return uuid, err
}

func (f *fakeFNS) ListIncomes(context.Context, time.Time, time.Time) ([]moynalog.IncomeRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, false, f.listErr
	}
	return append([]moynalog.IncomeRecord(nil), f.incomes...), !f.incomplete, nil
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
// На время обращения строка — unknown: процесс, упавший после приёма дохода
// ФНС, не должен оставить pending, который пробился бы вторым чеком.
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
	if seen == nil || seen.State != model.ReceiptUnknown {
		t.Fatalf("в момент обращения к ФНС строка чека должна уже лежать в unknown, got %+v", seen)
	}
}

// Процесс упал, пока ФНС принимала доход: исход не записан. Плановый проход
// второй чек не пробивает, а сверяет по метке и находит первый.
func TestReceiptPass_CrashDuringCreateNoSecondReceipt(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	claimRow(t, fs, "yk-crash", "ccc333")
	// Падение: ФНС приняла доход, а запись исхода не случилась — строка
	// остаётся такой, какой была в момент обращения.
	var inFlight model.Receipt
	fns.before = func(moynalog.Income) {
		r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-crash")
		inFlight = *r
	}
	a.receiptPassOnce(ctx)
	fs.mu.Lock()
	*fs.receipts[model.PayMethodYooKassa+"/yk-crash"] = inFlight
	fs.receipts[model.PayMethodYooKassa+"/yk-crash"].UpdatedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	fs.mu.Unlock()
	fns.before = nil

	a.receiptPassOnce(ctx)
	if n := len(fns.calls()); n != 1 {
		t.Fatalf("после падения в ФНС ушло %d чеков", n)
	}
	if r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-crash"); r.State != model.ReceiptCreated || r.UUID != "uuid-1" {
		t.Fatalf("сверка не нашла первый чек: %+v", r)
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

// claimRow кладёт в таблицу строку чека так, будто бот застолбил её и упал
// до ФНС.
func claimRow(t *testing.T, fs *fakeStore, extID, marker string) {
	t.Helper()
	ok, err := fs.ClaimReceipt(context.Background(), &model.Receipt{
		Provider: model.PayMethodYooKassa, ExtID: extID, AmountKopecks: 15000,
		Name: "VPN (" + marker + ")", Marker: marker, IncomeAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil || !ok {
		t.Fatalf("строка чека: ok=%v err=%v", ok, err)
	}
}

func setReceiptState(fs *fakeStore, extID, state, createdAt string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	r := fs.receipts[model.PayMethodYooKassa+"/"+extID]
	r.State = state
	if createdAt != "" {
		r.CreatedAt = createdAt
	}
}

func receiptAlerts(fm *fakeMsg) int {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	n := 0
	for _, t := range fm.texts {
		if strings.HasPrefix(t, "🧾 Чек «Мой налог»") {
			n++
		}
	}
	return n
}

// Чек, застолблённый до перезапуска и так и не дошедший до ФНС, добивает
// плановый проход.
func TestReceiptPass_PendingAfterRestart(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	claimRow(t, fs, "yk-restart", "aaa111")

	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-restart")
	if len(fns.calls()) != 1 || r.State != model.ReceiptCreated || r.UUID != "uuid-1" {
		t.Fatalf("вызовов %d, строка %+v", len(fns.calls()), r)
	}
	if c := fns.calls()[0]; c.Name != "VPN (aaa111)" || c.Amount != 150 {
		t.Fatalf("в ФНС ушло не то: %+v", c)
	}
	// Пробитый чек следующий проход не трогает.
	a.receiptPassOnce(ctx)
	if len(fns.calls()) != 1 {
		t.Fatalf("пробитый чек ушёл в ФНС снова: %d", len(fns.calls()))
	}
}

// ФНС была недоступна при оплате — чек добивает следующий проход.
func TestReceiptPass_RetriesAfterFNSDown(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	fns.answer = fnsDown
	if err := a.finalizeTopUp(ctx, 555, 15000, model.PayMethodYooKassa, "150 ₽", "yk-down"); err != nil {
		t.Fatal(err)
	}
	a.moneyWG.Wait()
	fns.answer = nil
	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-down")
	if len(fns.calls()) != 2 || r.State != model.ReceiptCreated || r.UUID != "uuid-2" {
		t.Fatalf("вызовов %d, строка %+v", len(fns.calls()), r)
	}
}

// Ответ потерян, а чек у ФНС есть: сверка находит его по метке и записывает
// uuid без второго пробития.
func TestReceiptPass_UnknownFoundByMarker(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	fns.answer = fnsLost
	if err := a.finalizeTopUp(ctx, 555, 15000, model.PayMethodYooKassa, "150 ₽", "yk-lost"); err != nil {
		t.Fatal(err)
	}
	a.moneyWG.Wait()
	// Чужой чек в кабинете с другой меткой сверку не сбивает.
	fns.incomes = append(fns.incomes, moynalog.IncomeRecord{UUID: "чужой", Names: []string{"VPN (zzz999)"}})
	fns.answer = nil

	// Свежий unknown не сверяется: доход мог ещё не попасть в список.
	a.receiptPassOnce(ctx)
	if r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-lost"); fns.lists != 0 || r.State != model.ReceiptUnknown {
		t.Fatalf("свежий unknown сверен: списков %d, строка %+v", fns.lists, r)
	}
	fs.mu.Lock()
	fs.receipts[model.PayMethodYooKassa+"/yk-lost"].UpdatedAt = time.Now().UTC().Add(-reconcileDelay).Format(time.RFC3339)
	fs.mu.Unlock()

	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-lost")
	if len(fns.calls()) != 1 || r.State != model.ReceiptCreated || r.UUID != "uuid-1" {
		t.Fatalf("вызовов %d, строка %+v", len(fns.calls()), r)
	}
}

// Список доходов полный, а чека в нём нет — ФНС его не приняла: пробиваем
// заново тем же наименованием с той же меткой.
func TestReceiptPass_UnknownNotInCompleteListReissued(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	claimRow(t, fs, "yk-gone", "bbb222")
	setReceiptState(fs, "yk-gone", model.ReceiptUnknown, "")

	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-gone")
	calls := fns.calls()
	if fns.lists != 1 || len(calls) != 1 || calls[0].Name != "VPN (bbb222)" || r.State != model.ReceiptCreated || r.UUID != "uuid-1" {
		t.Fatalf("списков %d, вызовов %+v, строка %+v", fns.lists, calls, r)
	}
}

// Список доходов неполный или не получен — чек остаётся unknown, в ФНС
// ничего не уходит: «нет в списке» тут ничего не значит.
func TestReceiptPass_UnknownIncompleteListWaits(t *testing.T) {
	for name, set := range map[string]func(*fakeFNS){
		"неполный список": func(f *fakeFNS) { f.incomplete = true },
		"ошибка списка":   func(f *fakeFNS) { f.listErr = &moynalog.Error{Kind: moynalog.KindRetry, Msg: "status 503"} },
	} {
		t.Run(name, func(t *testing.T) {
			a, _, fs, fns := receiptApp(t)
			ctx := context.Background()
			claimRow(t, fs, "yk-wait", "ccc333")
			setReceiptState(fs, "yk-wait", model.ReceiptUnknown, "")
			set(fns)

			a.receiptPassOnce(ctx)
			r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-wait")
			if fns.lists != 1 || len(fns.calls()) != 0 || r.State != model.ReceiptUnknown || r.LastError == "" {
				t.Fatalf("списков %d, вызовов %d, строка %+v", fns.lists, len(fns.calls()), r)
			}
		})
	}
}

// Несколько чеков с одной меткой — автоматика не угадывает: чек остаётся
// unknown, второй раз не пробивается.
func TestReceiptPass_UnknownAmbiguousWaits(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	claimRow(t, fs, "yk-amb", "ddd444")
	setReceiptState(fs, "yk-amb", model.ReceiptUnknown, "")
	fns.incomes = []moynalog.IncomeRecord{
		{UUID: "u1", Names: []string{"VPN (ddd444)"}},
		{UUID: "u2", Names: []string{"VPN (ddd444)"}},
	}
	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-amb")
	if len(fns.calls()) != 0 || r.State != model.ReceiptUnknown || r.UUID != "" {
		t.Fatalf("вызовов %d, строка %+v", len(fns.calls()), r)
	}
}

// История платежей, перенесённая из старого бота, строк чеков не имеет —
// проход идёт только по таблице чеков и ничего не пробивает.
func TestReceiptPass_ImportedHistoryUntouched(t *testing.T) {
	a, _, fs, fns := receiptApp(t)
	ctx := context.Background()
	for _, ext := range []string{"yk-old-1", "yk-old-2"} {
		if err := fs.AddPayment(ctx, &model.Payment{
			TelegramID: 555, Method: model.PayMethodYooKassa, Months: 1, Amount: "150.00 RUB",
			Status: model.PaymentPaid, ExtID: ext, CreatedAt: "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	a.receiptPassOnce(ctx)
	a.moneyWG.Wait()
	if len(fns.calls()) != 0 || fns.lists != 0 || len(fs.receipts) != 0 {
		t.Fatalf("история породила чеки: вызовов %d, списков %d, строк %d", len(fns.calls()), fns.lists, len(fs.receipts))
	}
}

// Отклонённый ФНС чек — один алерт владельцу, сколько бы проходов ни было.
func TestReceiptAlert_RejectedOnce(t *testing.T) {
	a, fm, fs, fns := receiptApp(t)
	ctx := context.Background()
	fns.answer = fnsRejected
	if err := a.finalizeTopUp(ctx, 555, 15000, model.PayMethodYooKassa, "150 ₽", "yk-rej"); err != nil {
		t.Fatal(err)
	}
	a.moneyWG.Wait()
	if n := receiptAlerts(fm); n != 1 {
		t.Fatalf("алерт об отказе должен уйти сразу и один, ушло %d", n)
	}
	a.receiptPassOnce(ctx)
	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-rej")
	if n := receiptAlerts(fm); n != 1 || len(fns.calls()) != 1 || r.AlertedAt == "" {
		t.Fatalf("алертов %d, вызовов %d, строка %+v", n, len(fns.calls()), r)
	}
}

// Отказ, по которому алерт не ушёл (бот упал между записью исхода и
// алертом), — алерт шлёт проход, тоже один.
func TestReceiptAlert_RejectedWithoutAlertCaughtByPass(t *testing.T) {
	a, fm, fs, fns := receiptApp(t)
	ctx := context.Background()
	claimRow(t, fs, "yk-rej2", "eee555")
	setReceiptState(fs, "yk-rej2", model.ReceiptRejected, "")
	a.receiptPassOnce(ctx)
	a.receiptPassOnce(ctx)
	if n := receiptAlerts(fm); n != 1 || len(fns.calls()) != 0 {
		t.Fatalf("алертов %d, вызовов %d", n, len(fns.calls()))
	}
}

// Алерт, не принятый Telegram, отметку не оставляет: следующий проход его
// повторяет, и доставленный — ровно один.
func TestReceiptAlert_UndeliveredRetried(t *testing.T) {
	a, fm, fs, _ := receiptApp(t)
	ctx := context.Background()
	claimRow(t, fs, "yk-lost", "hhh888")
	setReceiptState(fs, "yk-lost", model.ReceiptRejected, "")
	fm.mu.Lock()
	fm.kbFail = true
	fm.mu.Unlock()
	a.receiptPassOnce(ctx)
	if r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-lost"); r.AlertedAt != "" {
		t.Fatalf("недоставленный алерт оставил отметку: %+v", r)
	}
	fm.mu.Lock()
	fm.kbFail = false
	fm.mu.Unlock()
	before := receiptAlerts(fm)
	a.receiptPassOnce(ctx)
	a.receiptPassOnce(ctx)
	r, _ := fs.GetReceipt(ctx, model.PayMethodYooKassa, "yk-lost")
	if n := receiptAlerts(fm) - before; n != 1 || r.AlertedAt == "" {
		t.Fatalf("доставленных алертов %d, строка %+v", n, r)
	}
}

// Чек, не ставший created за сутки, — один алерт; попытки при этом
// продолжаются, и свежий чек алерта не даёт.
func TestReceiptAlert_StuckOnce(t *testing.T) {
	a, fm, fs, fns := receiptApp(t)
	ctx := context.Background()
	fns.answer = fnsDown
	claimRow(t, fs, "yk-stuck", "fff666")
	setReceiptState(fs, "yk-stuck", model.ReceiptPending, time.Now().UTC().Add(-25*time.Hour).Format(time.RFC3339))
	claimRow(t, fs, "yk-fresh", "ggg777")

	a.receiptPassOnce(ctx)
	a.receiptPassOnce(ctx)
	if n := receiptAlerts(fm); n != 1 {
		t.Fatalf("алертов %d, ожидался один", n)
	}
	if n := len(fns.calls()); n != 4 {
		t.Fatalf("оба чека должны пробоваться на каждом проходе: вызовов %d", n)
	}
	fm.mu.Lock()
	last := fm.texts[len(fm.texts)-1]
	fm.mu.Unlock()
	if !strings.Contains(last, "yk-stuck") || !strings.Contains(last, "150") {
		t.Fatalf("алерт не о том чеке: %q", last)
	}
}
