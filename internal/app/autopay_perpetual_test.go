package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remnabot/internal/model"
	"remnabot/internal/remnawave"
	"remnabot/internal/yookassa"
)

// autoPayPanelApp — магазин с автоплатежами, ЮKassa считает списания, а
// панель отдаёт человеку срок panelExpire.
func autoPayPanelApp(t *testing.T, uid int64, panelExpire string) (*App, *fakeStore, *atomic.Int32) {
	t.Helper()
	var charges atomic.Int32
	yk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/payments") {
			charges.Add(1)
			_, _ = w.Write([]byte(`{"id":"ap-pay","status":"canceled","paid":false,"amount":{"value":"150.00","currency":"RUB"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(yk.Close)
	oldBase := yookassa.BaseURL
	yookassa.BaseURL = yk.URL
	t.Cleanup(func() { yookassa.BaseURL = oldBase })

	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":[{"uuid":"u-1","username":"u1","status":"ACTIVE","expireAt":"` + panelExpire +
			`","subscriptionUrl":"https://sub.example/x","telegramId":` + itoa64(uid) + `}]}`))
	}))
	t.Cleanup(panel.Close)

	a, fs := autoPayApp(t)
	a.botCfg.Pricing = model.Pricing{Currency: "RUB", Base: map[int]string{1: "150"}}
	a.botCfg.NormalizePricing()
	a.panel = remnawave.New(model.PanelConfig{Mode: model.ModeRemote, BaseURL: panel.URL, APIToken: "t"})
	return a, fs, &charges
}

// Владелец сделал человека Бессрочным, сдвинув срок в панели; в базе бота
// ещё старый близкий срок. Планировщик не списывает и выключает
// автопродление.
func TestAutoPay_PerpetualByPanelNotCharged(t *testing.T) {
	const uid = int64(1000000091)
	a, fs, charges := autoPayPanelApp(t, uid, perpetual2099)
	ctx := context.Background()
	_ = fs.UpsertUser(ctx, uid)
	_ = fs.SetSubExpiry(ctx, uid, time.Now().UTC().Add(6*time.Hour).Format(time.RFC3339), "paid")
	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: uid, Method: model.PayMethodYooKassa, MethodID: "pm", Title: "•••• 1234", Months: 1, Enabled: true})

	a.autoPayOnce(ctx)

	if n := charges.Load(); n != 0 {
		t.Fatalf("бессрочному ушло %d списаний", n)
	}
	if ap, _ := fs.GetAutoPay(ctx, uid); ap == nil || ap.Enabled {
		t.Fatalf("автопродление бессрочного должно выключиться, карта — остаться: %+v", ap)
	}
}

// Бессрочный с включённым автопродлением может его выключить и отвязать
// карту, но не включить снова.
func TestAutoPay_PerpetualCanTurnOff(t *testing.T) {
	const uid = int64(1000000092)
	a, fs, _ := autoPayPanelApp(t, uid, perpetual2099)
	fm := a.msg.(*fakeMsg)
	ctx := context.Background()
	_ = fs.UpsertUser(ctx, uid)
	_ = fs.SetSubExpiry(ctx, uid, perpetual2099, "paid")
	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: uid, Method: model.PayMethodYooKassa, MethodID: "pm", Title: "•••• 1234", Months: 1, Enabled: true})

	a.onAutoPayUser(ctx, uid, "show")
	if !hasCallback(fm, "ap:off") || !hasCallback(fm, "ap:unlink") {
		t.Fatalf("бессрочному с включённым автопродлением нужны «Выключить» и «Отвязать»: %+v", fm.lastKB)
	}

	a.onAutoPayUser(ctx, uid, "off")
	if hasCallback(fm, "ap:on") {
		t.Fatalf("после выключения бессрочному предложено включить снова: %+v", fm.lastKB)
	}
	a.onAutoPayUser(ctx, uid, "show")
	if hasCallback(fm, "ap:on") || hasCallback(fm, "menu:buy") {
		t.Fatalf("бессрочному включать автопродление и покупать нечего: %+v", fm.lastKB)
	}
	if !hasCallback(fm, "ap:unlink") {
		t.Fatalf("отвязка карты бессрочному доступна: %+v", fm.lastKB)
	}
}

// Бессрочный оплатил старый счёт с сохранением карты: записи автопродления
// и предложения его подключить нет.
func TestAutoPay_PerpetualNotSavedFromPayment(t *testing.T) {
	const uid = int64(1000000093)
	a, fs, _ := autoPayPanelApp(t, uid, perpetual2099)
	fm := a.msg.(*fakeMsg)
	ctx := context.Background()
	_ = fs.UpsertUser(ctx, uid)
	_ = fs.SetSubExpiry(ctx, uid, perpetual2099, "paid")

	pay := &yookassa.Payment{ID: "pay_old"}
	pay.Metadata = map[string]string{"autopay": "1"}
	pay.PaymentMethod.ID = "pm_new"
	pay.PaymentMethod.Saved = true
	n0 := len(fm.texts)
	a.saveAutoPayFromPayment(ctx, uid, 1, pay, nil)

	if ap, _ := fs.GetAutoPay(ctx, uid); ap != nil {
		t.Fatalf("бессрочному создана запись автопродления: %+v", ap)
	}
	if len(fm.texts) != n0 {
		t.Fatalf("бессрочному предложено автопродление: %q", fm.texts[n0:])
	}
}
