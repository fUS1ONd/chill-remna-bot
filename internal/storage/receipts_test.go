package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"remnabot/internal/model"
)

func sampleReceipt(provider, extID string) *model.Receipt {
	return &model.Receipt{
		Provider:      provider,
		ExtID:         extID,
		AmountKopecks: 99000,
		Name:          "VPN (abc123)",
		Marker:        "abc123",
		IncomeAt:      "2026-11-30T21:00:00Z",
	}
}

// Право на чек столбится вставкой строки: на один платёж провайдера —
// ровно одна строка, сколько бы путей ни пытались застолбить его разом.
func TestReceiptClaimUnique(t *testing.T) {
	eachStore(t, func(t *testing.T, st Storage) {
		ctx := context.Background()

		const racers = 8
		var wg sync.WaitGroup
		wins := make(chan bool, racers)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := st.ClaimReceipt(ctx, sampleReceipt(model.PayMethodYooKassa, "pay-1"))
				if err != nil {
					t.Errorf("столбление: %v", err)
				}
				wins <- ok
			}()
		}
		wg.Wait()
		close(wins)
		won := 0
		for ok := range wins {
			if ok {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("застолбить один платёж должен ровно один путь, а удалось %d", won)
		}

		// Ключ — пара «провайдер + id»: тот же id у другого провайдера — другой чек.
		if ok, err := st.ClaimReceipt(ctx, sampleReceipt("other", "pay-1")); err != nil || !ok {
			t.Fatalf("тот же id у другого провайдера: ok=%v err=%v", ok, err)
		}

		r, err := st.GetReceipt(ctx, model.PayMethodYooKassa, "pay-1")
		if err != nil || r == nil {
			t.Fatalf("GetReceipt: %+v err=%v", r, err)
		}
		if r.State != model.ReceiptPending || r.Attempts != 0 || r.UUID != "" ||
			r.AmountKopecks != 99000 || r.Name != "VPN (abc123)" || r.Marker != "abc123" ||
			r.IncomeAt != "2026-11-30T21:00:00Z" || r.CreatedAt == "" {
			t.Fatalf("застолбленная строка: %+v", r)
		}
		if r, _ := st.GetReceipt(ctx, model.PayMethodYooKassa, "нет-такого"); r != nil {
			t.Fatalf("несуществующий чек должен читаться как nil, got %+v", r)
		}
	})
}

// Исход каждого обращения к ФНС записывается: состояние, uuid, ошибка, счётчик.
func TestReceiptResult(t *testing.T) {
	eachStore(t, func(t *testing.T, st Storage) {
		ctx := context.Background()
		if _, err := st.ClaimReceipt(ctx, sampleReceipt(model.PayMethodYooKassa, "pay-2")); err != nil {
			t.Fatal(err)
		}

		if err := st.SetReceiptResult(ctx, model.PayMethodYooKassa, "pay-2", model.ReceiptUnknown, "", "связь оборвалась"); err != nil {
			t.Fatal(err)
		}
		r, _ := st.GetReceipt(ctx, model.PayMethodYooKassa, "pay-2")
		if r == nil || r.State != model.ReceiptUnknown || r.Attempts != 1 || r.LastError != "связь оборвалась" || r.UpdatedAt == "" {
			t.Fatalf("после потерянного ответа: %+v", r)
		}

		if err := st.SetReceiptResult(ctx, model.PayMethodYooKassa, "pay-2", model.ReceiptCreated, "uuid-1", ""); err != nil {
			t.Fatal(err)
		}
		r, _ = st.GetReceipt(ctx, model.PayMethodYooKassa, "pay-2")
		if r == nil || r.State != model.ReceiptCreated || r.UUID != "uuid-1" || r.Attempts != 2 || r.LastError != "" {
			t.Fatalf("после пробития: %+v", r)
		}
	})
}

// Момент списания пишется обеими записями платежа и читается обратно; у
// платежа без него — пусто.
func TestPaymentCapturedAt(t *testing.T) {
	eachStore(t, func(t *testing.T, st Storage) {
		ctx := context.Background()
		const captured = "2026-10-31T20:59:30Z"
		if err := st.AddPayment(ctx, &model.Payment{
			TelegramID: 555, Method: model.PayMethodYooKassa, Months: 1, Amount: "150.00 RUB",
			Status: model.PaymentPaid, ExtID: "yk-buy", CapturedAt: captured,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddPaymentAndBalance(ctx, &model.Payment{
			TelegramID: 555, Method: model.PayMethodYooKassa, Amount: "100.00 RUB",
			Status: model.PaymentPaid, ExtID: "yk-top", Comment: "topup", CapturedAt: captured,
		}, 10000); err != nil {
			t.Fatal(err)
		}
		if err := st.AddPayment(ctx, &model.Payment{
			TelegramID: 555, Method: model.PayMethodStars, Months: 1, Amount: "100 ⭐",
			Status: model.PaymentPaid, ExtID: "st-1",
		}); err != nil {
			t.Fatal(err)
		}
		items, _, err := st.ListPayments(ctx, 10, 0)
		if err != nil || len(items) != 3 {
			t.Fatalf("ListPayments: len=%d err=%v", len(items), err)
		}
		for _, p := range items {
			want := captured
			if p.ExtID == "st-1" {
				want = ""
			}
			if p.CapturedAt != want {
				t.Fatalf("платёж %s: момент списания %q, ожидался %q", p.ExtID, p.CapturedAt, want)
			}
		}
		paid, err := st.PaidPayments(ctx)
		if err != nil || len(paid) != 3 {
			t.Fatalf("PaidPayments: len=%d err=%v", len(paid), err)
		}
		for _, p := range paid {
			if p.ExtID == "yk-buy" && p.CapturedAt != captured {
				t.Fatalf("PaidPayments потерял момент списания: %+v", p)
			}
		}
		// Выгрузка уносит момент списания вместе с платежом.
		snap, err := st.Export(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, p := range snap.Payments {
			if p.CapturedAt == captured {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("в выгрузке платежей с моментом списания %d, ожидалось 2", n)
		}
	})
}

// Миграция добавляет колонку к базе на прежней схеме: платежи, записанные до
// неё, читаются с пустым моментом списания.
func TestPaymentCapturedAtMigration(t *testing.T) {
	ctx := context.Background()
	st, err := Open(model.DBSQLite, filepath.Join(t.TempDir(), "old.db"), testCrypter(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := baseOf(t, st)
	migrateUpTo(t, b, 78)
	if _, err := b.db.ExecContext(ctx,
		"INSERT INTO payments (id, telegram_id, method, months, amount, status, comment, ext_id, created_at, plan_snapshot) "+
			"VALUES (1, 555, 'yookassa', 1, '150.00 RUB', 'paid', '', 'yk-old', '2026-10-01T00:00:00Z', '')"); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	items, _, err := st.ListPayments(ctx, 10, 0)
	if err != nil || len(items) != 1 || items[0].ExtID != "yk-old" || items[0].CapturedAt != "" {
		t.Fatalf("старый платёж после миграции: %+v, err=%v", items, err)
	}
}
