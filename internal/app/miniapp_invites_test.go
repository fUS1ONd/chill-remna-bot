package app

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"remnabot/internal/model"
)

// Текст отказа мини-аппа и кабинета — тот же, что в чате, без разметки.
func TestMiniDenyText(t *testing.T) {
	a, _, _ := uinvApp(t)
	ctx := context.Background()
	if got := a.MiniDenyText(ctx, uinvFriend); got != "✉️ Бот закрытый — войти можно только по приглашению от друга. Попросите ссылку у знакомого, который уже пользуется ботом." {
		t.Fatalf("с тумблером: %q", got)
	}
	a.botCfg.UserInvites = false
	if got := a.MiniDenyText(ctx, uinvFriend); got != "✉️ Бот работает по приглашениям. Попросите у администратора ссылку-приглашение — она откроет доступ автоматически." {
		t.Fatalf("без тумблера: %q", got)
	}
	a.setAccessMode(ctx, model.AccessWhitelist, false)
	if got := a.MiniDenyText(ctx, uinvFriend); got == "" {
		t.Fatal("в белом списке пустой отказ")
	}
}

// Экран приглашений мини-аппа и кабинета — тот же, что в чате: бонусы,
// квоты, создание со ссылками для «Поделиться» и «Скопировать», отзыв.
func TestMiniInvites_CreateShareRevoke(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	a.botCfg.Cabinet.Enabled = true
	a.botCfg.Webhook.PublicBaseURL = "https://lk.example.org"

	d := a.MiniInvites(ctx, uinvAuthor)
	if !d.Allowed || len(d.Invites) != 0 || d.MaxActive != 3 || d.TTLDays != 30 || d.Bonus == nil || d.Bonus.BonusValue != 50 {
		t.Fatalf("пустой экран: %+v", d)
	}
	d = a.MiniInviteCreate(ctx, uinvAuthor)
	if !d.OK || d.Message != "✅ Приглашение создано. Нажмите «📤 Поделиться» и выберите чат друга." || len(d.Invites) != 1 || d.Active != 1 {
		t.Fatalf("создание: %+v", d)
	}
	inv := d.Invites[0]
	if inv.Link != "https://t.me/testbot?start=inv_"+inv.Code || !strings.HasPrefix(inv.CabinetLink, "https://lk.example.org") ||
		!strings.HasSuffix(inv.CabinetLink, "?inv="+inv.Code) || inv.ExpireAt == "" {
		t.Fatalf("ссылки приглашения: %+v", inv)
	}
	share, err := url.Parse(inv.ShareURL)
	if err != nil || share.Host != "t.me" || share.Path != "/share/url" || share.Query().Get("url") != inv.Link ||
		!strings.Contains(share.Query().Get("text"), inv.CabinetLink) {
		t.Fatalf("Поделиться: %s", inv.ShareURL)
	}
	if !strings.Contains(inv.ShareText, inv.Link) || !strings.Contains(inv.ShareText, inv.CabinetLink) {
		t.Fatalf("текст для копирования: %q", inv.ShareText)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "ref_") || strings.Contains(string(b), "?ref=") {
		t.Fatalf("на экране приглашений ref_: %s", b)
	}

	d = a.MiniInviteRevoke(ctx, uinvAuthor, inv.Code)
	if !d.OK || d.Message != "🚫 Приглашение отозвано — ссылка больше не работает." || len(d.Invites) != 0 {
		t.Fatalf("отзыв: %+v", d)
	}
	if got, _ := fs.GetInvite(ctx, inv.Code); !got.Revoked {
		t.Fatal("приглашение не отозвано в базе")
	}
	d = a.MiniInviteRevoke(ctx, uinvAuthor, inv.Code)
	if d.OK || d.Error != "⚠️ Это приглашение уже использовано или отозвано." {
		t.Fatalf("повторный отзыв: %+v", d)
	}
}

// Лимит активных отказывает тем же текстом, что в чате; чужое не отзывается.
func TestMiniInvites_LimitAndForeign(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if d := a.MiniInviteCreate(ctx, uinvAuthor); !d.OK {
			t.Fatalf("создание %d: %+v", i, d)
		}
	}
	d := a.MiniInviteCreate(ctx, uinvAuthor)
	if d.OK || d.Error != "⚠️ У вас уже 3 активных приглашения. Отзовите одно или дождитесь, пока друг им воспользуется." || len(d.Invites) != 3 {
		t.Fatalf("четвёртое: %+v", d)
	}
	const other = int64(1000000502)
	_ = fs.UpsertUser(ctx, other)
	_ = fs.SetWhitelisted(ctx, other, true)
	if d := a.MiniInviteRevoke(ctx, other, d.Invites[0].Code); d.OK {
		t.Fatalf("отозвано чужое: %+v", d)
	}
	if n := len(activeInvitesOf(t, fs, uinvAuthor)); n != 3 {
		t.Fatalf("активных у Автора: %d", n)
	}
}

// Без права приглашать — отказ на экране, создание ничего не создаёт.
func TestMiniInvites_NotAllowed(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.UserInvites = false
	d := a.MiniInvites(ctx, uinvAuthor)
	if d.Allowed || d.Error != "⛔️ Приглашать друзей сейчас нельзя." {
		t.Fatalf("экран без права: %+v", d)
	}
	if d := a.MiniInviteCreate(ctx, uinvAuthor); d.OK || d.Allowed {
		t.Fatalf("создание без права: %+v", d)
	}
	if n := len(activeInvitesOf(t, fs, uinvAuthor)); n != 0 {
		t.Fatalf("создано без права: %d", n)
	}
}

// В закрытом режиме с тумблером рефералка мини-аппа и кабинета не отдаёт
// ref_-ссылок, а меню ведёт на экран приглашений; без тумблера — как в chill.
func TestMiniReferral_NoRefWithToggle(t *testing.T) {
	a, _, _ := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	a.botCfg.Cabinet.Enabled = true
	a.botCfg.Webhook.PublicBaseURL = "https://lk.example.org"

	for _, id := range []int64{uinvAuthor, uinvFriend} {
		b, _ := json.Marshal(a.MiniReferral(ctx, id))
		if strings.Contains(string(b), "ref_") || strings.Contains(string(b), "?ref=") {
			t.Fatalf("рефералка %d с тумблером: %s", id, b)
		}
	}
	m := a.MiniMenu(ctx, uinvAuthor, false)
	if !m.UserInvites || m.ReferralOn {
		t.Fatalf("меню Автора: user_invites=%v referral_on=%v", m.UserInvites, m.ReferralOn)
	}
	if m := a.MiniMenu(ctx, uinvFriend, true); m.UserInvites || m.ReferralOn {
		t.Fatalf("меню без права: user_invites=%v referral_on=%v", m.UserInvites, m.ReferralOn)
	}

	a.botCfg.UserInvites = false
	if r := a.MiniReferral(ctx, uinvAuthor); !strings.Contains(r.Link, "ref_") {
		t.Fatalf("без тумблера нет ref_: %+v", r)
	}
	if m := a.MiniMenu(ctx, uinvAuthor, false); m.UserInvites || !m.ReferralOn {
		t.Fatalf("меню без тумблера: user_invites=%v referral_on=%v", m.UserInvites, m.ReferralOn)
	}
}

// Старт мини-аппа с inv_ в start_param активирует Приглашение от
// пользователя так же, как /start в чате: Доступ, Автор — Пригласивший,
// бонус и уведомления; недействительное — отказ «попросите у друга новое».
func TestMiniStart_UserInvite(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	_ = fs.SetUserInfo(ctx, uinvAuthor, "", "Автор")
	userInvite(t, fs, "c1", uinvAuthor)

	if a.MiniStart(ctx, uinvFriend, "inv_c1", false) == "" {
		t.Fatal("нет текста об активации")
	}
	u, _ := fs.GetUser(ctx, uinvFriend)
	if u == nil || !u.Whitelisted || u.ReferredBy != uinvAuthor || a.MiniAccessDenied(ctx, uinvFriend) {
		t.Fatalf("друг после старта мини-аппа: %+v", u)
	}
	if inviteUsed(t, fs, "c1") != 1 {
		t.Fatal("активация не списана")
	}
	if au, _ := fs.GetUser(ctx, uinvAuthor); au.Balance != 5000 {
		t.Fatalf("бонус Автору: %d коп.", au.Balance)
	}
	if !strings.Contains(fm.joined(), "По вашему приглашению пришёл") {
		t.Fatalf("Автор не уведомлён: %s", fm.joined())
	}

	const stranger = int64(1000000701)
	userInvite(t, fs, "c2", uinvAuthor)
	a.botCfg.UserInvites = false
	if got := a.MiniStart(ctx, stranger, "inv_c2", false); got != "⛔️ Приглашение недействительно — попросите у друга новое." {
		t.Fatalf("приостановленное: %q", got)
	}
	if !a.MiniAccessDenied(ctx, stranger) || inviteUsed(t, fs, "c2") != 0 {
		t.Fatal("приостановленное приглашение пустило или потратилось")
	}
}
