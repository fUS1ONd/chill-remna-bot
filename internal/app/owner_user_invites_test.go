package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"remnabot/internal/model"
)

// lastKBData — callback_data кнопок последнего экрана.
func lastKBData(fm *fakeMsg) []string {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	var out []string
	for _, row := range fm.lastKB {
		for _, b := range row {
			if b.CallbackData != "" {
				out = append(out, b.CallbackData)
			}
		}
	}
	return out
}

// lastKBLabels — подписи кнопок последнего экрана.
func lastKBLabels(fm *fakeMsg) string {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	var out []string
	for _, row := range fm.lastKB {
		for _, b := range row {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "|")
}

func inviteIn(t *testing.T, fs *fakeStore, code string) *model.Invite {
	t.Helper()
	inv, err := fs.GetInvite(context.Background(), code)
	if err != nil || inv == nil {
		t.Fatalf("приглашение %s: %+v err=%v", code, inv, err)
	}
	return inv
}

// Свой список владельца — только его приглашения; Приглашения пользователей
// — в отдельном списке: активные, с Автором и счётчиками.
func TestOwnerUserInvites_Lists(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	fs.users[uinvAuthor].FirstName = "Автор"
	exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "adm1", MaxUses: 1, ExpiresAt: exp})
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "uact", MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor})
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "uused", MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor})
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "urev", MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor, Revoked: true})
	if ok, _ := fs.UseInvite(ctx, "uused", uinvFriend, false); !ok {
		t.Fatal("активация не прошла")
	}
	_ = fs.AddPayment(ctx, &model.Payment{TelegramID: uinvFriend, Method: "yk", Months: 1, Amount: "100", Status: model.PaymentPaid})

	a.handleCallback(ctx, cb(100, "menu:access"))
	if !hasCB(lastKBData(fm), "acc:ulist") {
		t.Fatalf("в разделе доступа нет списка Приглашений пользователей: %v", lastKBData(fm))
	}

	a.handleCallback(ctx, cb(100, "acc:list"))
	own := strings.Join(lastKBData(fm), " ")
	if !strings.Contains(own, "acc:show:adm1") || strings.Contains(own, "acc:show:u") {
		t.Fatalf("свой список владельца: %s", own)
	}

	a.handleCallback(ctx, cb(100, "acc:ulist"))
	list := strings.Join(lastKBData(fm), " ")
	if !strings.Contains(list, "acc:show:uact") || strings.Contains(list, "uused") ||
		strings.Contains(list, "urev") || strings.Contains(list, "adm1") {
		t.Fatalf("список Приглашений пользователей — только активные: %s", list)
	}
	if !strings.Contains(lastKBLabels(fm), "Автор") {
		t.Fatalf("в списке нет Автора: %s", lastKBLabels(fm))
	}
	screen := fm.last()
	for _, want := range []string{"Создано: <b>3</b>", "Активировано: <b>1</b>", "Из них оплатили: <b>1</b>"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("нет счётчика %q: %s", want, screen)
		}
	}

	// Чужой этот список не открывает.
	before := len(fm.allCallbackData())
	a.handleCallback(ctx, cb(uinvAuthor, "acc:ulist"))
	for _, d := range fm.allCallbackData()[before:] {
		if strings.HasPrefix(d, "acc:show:") {
			t.Fatal("список Приглашений пользователей открыт не владельцем")
		}
	}
}

// Список Приглашений пользователей листается страницами.
func TestOwnerUserInvites_Pages(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for i := 0; i < invitesPageSize+2; i++ {
		_ = fs.CreateInvite(ctx, &model.Invite{Code: "p" + itoa(i), MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor,
			CreatedAt: time.Now().UTC().Add(-time.Duration(i) * time.Minute).Format(time.RFC3339)})
	}
	a.handleCallback(ctx, cb(100, "acc:ulist"))
	first := lastKBData(fm)
	if !hasCB(first, "acc:upage:1") || hasCB(first, "acc:show:p"+itoa(invitesPageSize)) {
		t.Fatalf("первая страница: %v", first)
	}
	a.handleCallback(ctx, cb(100, "acc:upage:1"))
	second := lastKBData(fm)
	if !hasCB(second, "acc:show:p"+itoa(invitesPageSize)) || hasCB(second, "acc:show:p0") {
		t.Fatalf("вторая страница: %v", second)
	}
}

// Владелец отзывает любое Приглашение пользователя из его карточки; карточка
// показывает Автора и не даёт удалить приглашение.
func TestOwnerUserInvites_RevokeFromCard(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	fs.users[uinvAuthor].FirstName = "Автор"
	exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "uact", MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor})

	a.handleCallback(ctx, cb(100, "acc:show:uact"))
	if !strings.Contains(fm.last(), "Автор") {
		t.Fatalf("в карточке нет Автора: %s", fm.last())
	}
	data := lastKBData(fm)
	if !hasCB(data, "acc:revoke:uact") || hasCB(data, "acc:del:uact") || !hasCB(data, "acc:ulist") {
		t.Fatalf("кнопки карточки Приглашения пользователя: %v", data)
	}
	a.handleCallback(ctx, cb(100, "acc:revoke:uact"))
	if !inviteIn(t, fs, "uact").Revoked {
		t.Fatal("владелец не отозвал Приглашение пользователя")
	}
	if !strings.Contains(fm.last(), "Приглашения пользователей") || hasCB(lastKBData(fm), "acc:show:uact") {
		t.Fatalf("после отзыва не список Приглашений пользователей: %v", lastKBData(fm))
	}
}

// Карточка пользователя: Пригласивший, приглашённые им и его активные
// Приглашения с отзывом.
func TestOwnerUserInvites_UserCard(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	fs.users[uinvAuthor].FirstName = "Автор"
	_ = fs.UpsertUser(ctx, uinvFriend)
	fs.users[uinvFriend].FirstName = "Друг"
	_ = fs.SetReferredBy(ctx, uinvFriend, uinvAuthor)
	exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "uact", MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor})
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "urev", MaxUses: 1, ExpiresAt: exp, CreatedBy: uinvAuthor, Revoked: true})

	friend := itoa(int(uinvFriend))
	author := itoa(int(uinvAuthor))
	a.handleCallback(ctx, cb(100, "usr:view:"+friend))
	if !strings.Contains(fm.last(), "Пригласивший: Автор") {
		t.Fatalf("в карточке друга нет Пригласившего: %s", fm.last())
	}
	if !hasCB(lastKBData(fm), "usr:view:"+author) {
		t.Fatalf("нет перехода к Пригласившему: %v", lastKBData(fm))
	}

	a.handleCallback(ctx, cb(100, "usr:view:"+author))
	data := lastKBData(fm)
	if !hasCB(data, "usr:refs:"+author+":0") || !strings.Contains(lastKBLabels(fm), "Приглашённые (1)") {
		t.Fatalf("нет приглашённых Автором: %v %s", data, lastKBLabels(fm))
	}
	if !hasCB(data, "usr:urev:"+author+":uact") || hasCB(data, "usr:urev:"+author+":urev") {
		t.Fatalf("отзыв активных Приглашений Автора: %v", data)
	}

	a.handleCallback(ctx, cb(100, "usr:refs:"+author+":0"))
	if !hasCB(lastKBData(fm), "usr:view:"+friend) {
		t.Fatalf("в списке приглашённых нет друга: %v", lastKBData(fm))
	}

	a.handleCallback(ctx, cb(100, "usr:urev:"+author+":uact"))
	if !inviteIn(t, fs, "uact").Revoked {
		t.Fatal("отзыв из карточки Автора не прошёл")
	}
	if hasCB(lastKBData(fm), "usr:urev:"+author+":uact") || !hasCB(lastKBData(fm), "usr:refs:"+author+":0") {
		t.Fatalf("после отзыва не карточка Автора без отозванного: %v", lastKBData(fm))
	}

	// Отзыв из карточки трогает только Приглашения этого человека.
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "adm1", MaxUses: 1, ExpiresAt: exp})
	a.handleCallback(ctx, cb(100, "usr:urev:"+author+":adm1"))
	if inviteIn(t, fs, "adm1").Revoked {
		t.Fatal("из карточки Автора отозвано чужое приглашение")
	}
}
