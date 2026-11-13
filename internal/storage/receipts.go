package storage

import (
	"context"
	"database/sql"
	"errors"

	"remnabot/internal/model"
)

const receiptCols = "provider, ext_id, amount_kopecks, name, marker, income_at, receipt_uuid, state, attempts, last_error, alerted_at, created_at, updated_at"

func (b *base) ClaimReceipt(ctx context.Context, r *model.Receipt) (bool, error) {
	if r.CreatedAt == "" {
		r.CreatedAt = nowStr()
	}
	if r.State == "" {
		r.State = model.ReceiptPending
	}
	// ON CONFLICT DO NOTHING одинаково понимают SQLite и Postgres: проигравший
	// гонку получает не ошибку, а ноль вставленных строк.
	res, err := b.db.ExecContext(ctx,
		"INSERT INTO receipts (provider, ext_id, amount_kopecks, name, marker, income_at, state, created_at) "+
			"VALUES ("+b.ph(1)+", "+b.ph(2)+", "+b.ph(3)+", "+b.ph(4)+", "+b.ph(5)+", "+b.ph(6)+", "+b.ph(7)+", "+b.ph(8)+") "+
			"ON CONFLICT (provider, ext_id) DO NOTHING",
		r.Provider, r.ExtID, r.AmountKopecks, r.Name, r.Marker, r.IncomeAt, r.State, r.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (b *base) GetReceipt(ctx context.Context, provider, extID string) (*model.Receipt, error) {
	var r model.Receipt
	// #nosec G202 -- колонки из константы receiptCols, значения передаются биндовыми параметрами
	err := b.db.QueryRowContext(ctx,
		"SELECT "+receiptCols+" FROM receipts WHERE provider = "+b.ph(1)+" AND ext_id = "+b.ph(2), provider, extID).
		Scan(&r.Provider, &r.ExtID, &r.AmountKopecks, &r.Name, &r.Marker, &r.IncomeAt, &r.UUID, &r.State,
			&r.Attempts, &r.LastError, &r.AlertedAt, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (b *base) SetReceiptResult(ctx context.Context, provider, extID, state, uuid, lastError string) error {
	_, err := b.db.ExecContext(ctx,
		"UPDATE receipts SET state = "+b.ph(1)+", receipt_uuid = "+b.ph(2)+", last_error = "+b.ph(3)+
			", attempts = attempts + 1, updated_at = "+b.ph(4)+
			" WHERE provider = "+b.ph(5)+" AND ext_id = "+b.ph(6),
		state, uuid, lastError, nowStr(), provider, extID)
	return err
}
