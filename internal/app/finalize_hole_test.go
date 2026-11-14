package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"remnabot/internal/model"
	"remnabot/internal/remnawave"
	"remnabot/internal/storage"
)

// finalizeHoleApp — receiptApp с панелью, считающей продления: создание или
// правка пользователя (запись в /api/users) — это выдача или продление
// подписки.
func finalizeHoleApp(t *testing.T) (*App, *fakeMsg, *fakeStore, *fakeFNS, *atomic.Int32) {
	t.Helper()
	a, fm, fs, fns := receiptApp(t)
	var extends atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet && r.URL.Path == "/api/users" {
			extends.Add(1)
		}
		if strings.Contains(r.URL.Path, "/by-telegram-id/") {
			_, _ = w.Write([]byte(`{"response":[{"uuid":"u1","tag":"CHILLBOT","username":"tg_555","subscriptionUrl":"https://sub/x","expireAt":"2030-01-01T00:00:00Z"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"uuid":"u1","subscriptionUrl":"https://sub/x","expireAt":"2030-01-01T00:00:00Z"}}`))
	}))
	t.Cleanup(panel.Close)
	a.panel = remnawave.New(model.PanelConfig{Mode: model.ModeRemote, BaseURL: panel.URL, APIToken: "t"})
	return a, fm, fs, fns, &extends
}

func sawText(fm *fakeMsg, sub string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, s := range fm.texts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Касса деньги приняла, а записать платёж не удалось: финализация встаёт до
// панели и до чека, владелец получает алерт.
func TestFinalize_UnrecordedPaymentStopsBeforeExtendAndReceipt(t *testing.T) {
	a, fm, fs, fns, extends := finalizeHoleApp(t)
	ctx := context.Background()
	fs.addPaymentErr = errors.New("database is locked")

	_, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-hole", nil)
	a.moneyWG.Wait()

	if err == nil {
		t.Fatal("сбой записи платежа должен вернуться ошибкой — иначе вебхук ответит кассе «принято»")
	}
	if n := extends.Load(); n != 0 {
		t.Fatalf("подписка продлена %d раз, хотя платёж не записан", n)
	}
	if n := len(fns.calls()); n != 0 {
		t.Fatalf("ушло %d чеков по незаписанному платежу", n)
	}
	if !sawText(fm, "yk-hole") {
		t.Fatalf("владелец не получил алерт: %q", fm.texts)
	}
}

// Повторная доставка уведомления после сбоя записи проходит весь путь и
// продлевает подписку ровно один раз, с одним чеком.
func TestFinalize_RedeliveryAfterUnrecordedExtendsOnce(t *testing.T) {
	a, _, fs, fns, extends := finalizeHoleApp(t)
	ctx := context.Background()

	fs.addPaymentErr = errors.New("database is locked")
	_, _, _ = a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-hole", nil)
	fs.addPaymentErr = nil
	if _, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-hole", nil); err != nil {
		t.Fatalf("повторная доставка после сбоя должна финализировать платёж: %v", err)
	}
	a.moneyWG.Wait()

	if n := extends.Load(); n != 1 {
		t.Fatalf("подписка продлена %d раз, нужно ровно 1", n)
	}
	if n := len(fns.calls()); n != 1 {
		t.Fatalf("ушло %d чеков, нужен ровно 1", n)
	}
}

// Дубль по-прежнему тихо возвращается: без продления, без второго чека и без
// алерта владельцу.
func TestFinalize_DuplicateStaysQuiet(t *testing.T) {
	a, fm, _, fns, extends := finalizeHoleApp(t)
	ctx := context.Background()

	if _, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-dup", nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-dup", nil)
	a.moneyWG.Wait()

	if !errors.Is(err, storage.ErrDuplicateExtID) {
		t.Fatalf("повтор оплаченного платежа должен вернуть дубль, got %v", err)
	}
	if n := extends.Load(); n != 1 {
		t.Fatalf("подписка продлена %d раз, нужно ровно 1", n)
	}
	if n := len(fns.calls()); n != 1 {
		t.Fatalf("ушло %d чеков, нужен ровно 1", n)
	}
	if sawText(fm, "yk-dup") {
		t.Fatalf("дубль не повод для алерта: %q", fm.texts)
	}
}

// Барьер, поставленный до панели, не должен съесть платёж, если панель
// подписку не выдала: повторная доставка выдаёт её.
func TestFinalize_PanelFailureLeavesRedeliveryOpen(t *testing.T) {
	a, _, _, fns, extends := finalizeHoleApp(t)
	ctx := context.Background()
	good := a.panel
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	a.panel = remnawave.New(model.PanelConfig{Mode: model.ModeRemote, BaseURL: down.URL, APIToken: "t"})

	if _, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-pdown", nil); err == nil {
		t.Fatal("отказ панели должен вернуться ошибкой")
	}
	a.panel = good
	if _, _, err := a.finalizePurchase(ctx, 555, 1, model.PayMethodYooKassa, "150 ₽", "yk-pdown", nil); err != nil {
		t.Fatalf("после отказа панели повтор должен выдать подписку: %v", err)
	}
	a.moneyWG.Wait()

	if n := extends.Load(); n != 1 {
		t.Fatalf("подписка продлена %d раз, нужно ровно 1", n)
	}
	if n := len(fns.calls()); n != 1 {
		t.Fatalf("ушло %d чеков, нужен ровно 1", n)
	}
}
