package storage

import (
	"context"
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
