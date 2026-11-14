package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remnabot/internal/model"
)

// Приглашение помнит Автора и что оно выдаёт; список Автора — только его
// Приглашения, свежие первыми.
func TestUserInvitesAuthor(t *testing.T) {
	eachStore(t, func(t *testing.T, st Storage) {
		ctx := context.Background()
		_ = st.CreateInvite(ctx, &model.Invite{Code: "adm", MaxUses: 1})
		_ = st.CreateInvite(ctx, &model.Invite{Code: "u1", MaxUses: 1, CreatedBy: 501, CreatedAt: "2026-11-01T00:00:00Z"})
		_ = st.CreateInvite(ctx, &model.Invite{Code: "u2", MaxUses: 1, CreatedBy: 501, CreatedAt: "2026-11-02T00:00:00Z"})
		_ = st.CreateInvite(ctx, &model.Invite{Code: "other", MaxUses: 1, CreatedBy: 502})

		inv, err := st.GetInvite(ctx, "adm")
		if err != nil || inv == nil || inv.CreatedBy != 0 || inv.Grant != model.InviteGrantAccess {
			t.Fatalf("админское приглашение: %+v err=%v", inv, err)
		}
		inv, _ = st.GetInvite(ctx, "u1")
		if inv == nil || inv.CreatedBy != 501 || inv.Grant != model.InviteGrantAccess {
			t.Fatalf("приглашение пользователя: %+v", inv)
		}
		list, err := st.ListInvitesBy(ctx, 501)
		if err != nil || len(list) != 2 || list[0].Code != "u2" || list[1].Code != "u1" {
			t.Fatalf("список Автора: %+v err=%v", list, err)
		}
	})
}

// Автор отзывает только своё и неиспользованное; условие стоит в самом
// запросе, поэтому из пары «отзыв» и «активация» проходит первый.
func TestRevokeUserInviteRace(t *testing.T) {
	eachStore(t, func(t *testing.T, st Storage) {
		ctx := context.Background()
		_ = st.CreateInvite(ctx, &model.Invite{Code: "a", MaxUses: 1, CreatedBy: 501})
		_ = st.CreateInvite(ctx, &model.Invite{Code: "b", MaxUses: 1, CreatedBy: 501})

		if ok, err := st.RevokeInviteBy(ctx, "a", 502); err != nil || ok {
			t.Fatalf("чужое приглашение отозвано: ok=%v err=%v", ok, err)
		}
		// Отзыв первым — активация не проходит.
		if ok, err := st.RevokeInviteBy(ctx, "a", 501); err != nil || !ok {
			t.Fatalf("отзыв своего: ok=%v err=%v", ok, err)
		}
		if ok, _ := st.UseInvite(ctx, "a", 1000000001, false); ok {
			t.Fatal("отозванное приглашение активировалось")
		}
		// Активация первой — отзыв не проходит.
		if ok, err := st.UseInvite(ctx, "b", 1000000002, false); err != nil || !ok {
			t.Fatalf("активация: ok=%v err=%v", ok, err)
		}
		if ok, err := st.RevokeInviteBy(ctx, "b", 501); err != nil || ok {
			t.Fatalf("использованное приглашение отозвано: ok=%v err=%v", ok, err)
		}
		if inv, _ := st.GetInvite(ctx, "b"); inv == nil || inv.Revoked || inv.Used != 1 {
			t.Fatalf("использованное приглашение: %+v", inv)
		}
	})
}

// База на схеме v77: приглашения, созданные до миграции, становятся
// админскими и выдают только Доступ, остальное в них не меняется.
func TestInvitesAuthorMigration(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		st, err := Open(model.DBSQLite, filepath.Join(t.TempDir(), "old.db"), testCrypter(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		b := baseOf(t, st)
		migrateUpTo(t, b, 77)
		checkInvitesAuthorMigration(t, st, b)
	})
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		return
	}
	t.Run("postgres", func(t *testing.T) {
		ctx := context.Background()
		st, err := Open(model.DBPostgres, dsn, testCrypter(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		cleanPGData(t, dsn)
		// Откат к v77 для приглашений: колонок 0080/0081 и их версий нет.
		b := baseOf(t, st)
		for _, q := range []string{
			"ALTER TABLE invites DROP COLUMN created_by",
			"ALTER TABLE invites DROP COLUMN grant_kind",
			"DELETE FROM schema_migrations WHERE version IN (80, 81)",
		} {
			if _, err := b.db.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		checkInvitesAuthorMigration(t, st, b)
	})
}

func checkInvitesAuthorMigration(t *testing.T, st Storage, b *base) {
	t.Helper()
	ctx := context.Background()
	exp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if _, err := b.db.ExecContext(ctx,
		"INSERT INTO invites (code, max_uses, used, expires_at, created_at, revoked, note) "+
			"VALUES ('old', 5, 2, '"+exp+"', '2026-10-01T00:00:00Z', 1, 'заметка')"); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	inv, err := st.GetInvite(ctx, "old")
	want := model.Invite{Code: "old", MaxUses: 5, Used: 2, ExpiresAt: exp, CreatedAt: "2026-10-01T00:00:00Z",
		Revoked: true, Note: "заметка", CreatedBy: 0, Grant: model.InviteGrantAccess}
	if err != nil || inv == nil || *inv != want {
		t.Fatalf("старое приглашение после миграции: %+v err=%v", inv, err)
	}
}
