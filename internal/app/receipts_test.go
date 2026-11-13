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
