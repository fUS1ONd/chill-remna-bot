package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remnabot/internal/model"
	"remnabot/internal/yookassa"
)

// hasCallback — есть ли в последней клавиатуре кнопка с этим callback.
func hasCallback(fm *fakeMsg, data string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, row := range fm.lastKB {
		for _, b := range row {
			if b.CallbackData == data {
				return true
			}
		}
	}
	return false
}

// «🗑 Отвязать карту» есть на экране автопродления, пока способ оплаты
// сохранён, — и при включённом, и при выключенном автопродлении. Без карты
// отвязывать нечего.
func TestUnlink_ButtonOnlyWithSavedMethod(t *testing.T) {
	a, fs := autoPayApp(t)
	fm := a.msg.(*fakeMsg)
	ctx := context.Background()

	a.onAutoPayUser(ctx, 81, "show")
	if hasCallback(fm, "ap:unlink") {
		t.Fatal("без сохранённой карты кнопки отвязки быть не должно")
	}

	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: 81, Method: model.PayMethodYooKassa, MethodID: "pm", Title: "•••• 1234", Months: 1, Enabled: true})
	a.onAutoPayUser(ctx, 81, "show")
	if !hasCallback(fm, "ap:unlink") {
		t.Fatal("при включённом автопродлении нужна кнопка отвязки")
	}

	_ = fs.SetAutoPayEnabled(ctx, 81, false)
	a.onAutoPayUser(ctx, 81, "show")
	if !hasCallback(fm, "ap:unlink") {
		t.Fatal("при выключенном автопродлении с сохранённой картой нужна кнопка отвязки")
	}
}

// «Отвязать» спрашивает подтверждение с названием карты; «Отмена» ничего не
// трогает, «Да» удаляет запись автопродления целиком.
func TestUnlink_ConfirmCancelAndDelete(t *testing.T) {
	a, fs := autoPayApp(t)
	fm := a.msg.(*fakeMsg)
	ctx := context.Background()
	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: 82, Method: model.PayMethodYooKassa, MethodID: "pm", Title: "•••• 1234", Months: 1, Enabled: true})

	a.onAutoPayUser(ctx, 82, "unlink")
	if !sawText(fm, "Отвязать карту •••• 1234?") || !sawText(fm, "Автопродление выключится") {
		t.Fatalf("нет подтверждения с названием карты: %q", fm.texts)
	}
	if !hasCallback(fm, "ap:unlinkok") || !hasCallback(fm, "ap:show") {
		t.Fatalf("под вопросом нужны «Да» и «Отмена»: %+v", fm.lastKB)
	}
	if ap, _ := fs.GetAutoPay(ctx, 82); ap == nil {
		t.Fatal("сам вопрос карту отвязывать не должен")
	}

	// Отмена возвращает на экран автопродления, карта на месте.
	a.onAutoPayUser(ctx, 82, "show")
	if ap, _ := fs.GetAutoPay(ctx, 82); ap == nil || !ap.Enabled {
		t.Fatalf("после отмены запись должна остаться как была: %+v", ap)
	}

	a.onAutoPayUser(ctx, 82, "unlinkok")
	if ap, _ := fs.GetAutoPay(ctx, 82); ap != nil {
		t.Fatalf("после отвязки записи автопродления быть не должно: %+v", ap)
	}
	if !sawText(fm, "Карта отвязана") {
		t.Fatalf("нет ответа об отвязке: %q", fm.texts)
	}
	if hasCallback(fm, "ap:unlink") {
		t.Fatal("после отвязки кнопка отвязки показываться не должна")
	}
}

// Отвязка во время автосписания: пока списание в ЮKassa в полёте, отвязка
// ждёт его конца; после неё следующий проход планировщика не списывает.
func TestUnlink_DuringAutoChargeNoChargeAfter(t *testing.T) {
	var charges atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	yk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/payments") {
			charges.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			// Платёж не прошёл (карта отклонена) — финализации и панели нет.
			_, _ = w.Write([]byte(`{"id":"ap-pay","status":"canceled","paid":false,"amount":{"value":"150.00","currency":"RUB"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(yk.Close)
	oldBase := yookassa.BaseURL
	yookassa.BaseURL = yk.URL
	t.Cleanup(func() { yookassa.BaseURL = oldBase })
	// При провале теста обработчик не должен висеть: иначе yk.Close ждёт вечно.
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	a, fs := autoPayApp(t)
	a.botCfg.Pricing = model.Pricing{Currency: "RUB", Base: map[int]string{1: "150"}}
	a.botCfg.NormalizePricing()
	ctx := context.Background()
	_ = fs.UpsertUser(ctx, 83)
	_ = fs.SetSubExpiry(ctx, 83, time.Now().UTC().Add(6*time.Hour).Format(time.RFC3339), "paid")
	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: 83, Method: model.PayMethodYooKassa, MethodID: "pm", Title: "•••• 1234", Months: 1, Enabled: true})

	chargeDone := make(chan struct{})
	go func() { a.autoPayOnce(ctx); close(chargeDone) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("списание не началось")
	}

	unlinked := make(chan struct{})
	go func() { a.onAutoPayUser(ctx, 83, "unlinkok"); close(unlinked) }()
	select {
	case <-unlinked:
		t.Fatal("отвязка завершилась, пока списание в полёте: деньги спишутся уже после неё")
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	<-chargeDone
	select {
	case <-unlinked:
	case <-time.After(5 * time.Second):
		t.Fatal("отвязка не дождалась конца списания")
	}

	if ap, _ := fs.GetAutoPay(ctx, 83); ap != nil {
		t.Fatalf("после отвязки записи быть не должно: %+v", ap)
	}
	before := charges.Load()
	a.autoPayOnce(ctx)
	if n := charges.Load(); n != before {
		t.Fatalf("после отвязки ушло %d списаний", n-before)
	}
}

// После отвязки всё как у новичка: следующая оплата картой с сохранением
// приносит вопрос «Включить автопродление?», а списания до «да» не включены.
func TestUnlink_NextCardPaymentOffersAgain(t *testing.T) {
	a, fs := autoPayApp(t)
	fm := a.msg.(*fakeMsg)
	ctx := context.Background()
	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: 84, Method: model.PayMethodYooKassa, MethodID: "pm_old", Title: "•••• 1234", Months: 1, Enabled: true})

	a.onAutoPayUser(ctx, 84, "unlinkok")

	pay := &yookassa.Payment{ID: "pay_after"}
	pay.Metadata = map[string]string{"autopay": "1"}
	pay.PaymentMethod.ID = "pm_new"
	pay.PaymentMethod.Saved = true
	n0 := len(fm.texts)
	a.saveAutoPayFromPayment(ctx, 84, 1, pay, nil)

	offered := false
	for _, txt := range fm.texts[n0:] {
		if strings.Contains(txt, "Включить автопродление?") {
			offered = true
		}
	}
	if !offered || !hasCallback(fm, "ap:on") {
		t.Fatalf("после отвязки оплата картой должна снова предложить автопродление: %q", fm.texts[n0:])
	}
	if a.autoPayOn(ctx, 84) {
		t.Fatal("до согласия автопродление включаться не должно")
	}
}
