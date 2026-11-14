package app

import (
	"context"
	"strings"
	"testing"

	"remnabot/internal/model"
)

// cabInvApp — закрытый бот «по приглашениям» с Приглашениями от
// пользователей и включённым кабинетом без модерации новичков.
func cabInvApp(t *testing.T) (*App, *fakeMsg, *fakeStore) {
	t.Helper()
	a, fm, fs := uinvApp(t)
	a.botCfg.NormalizeCabinet()
	a.botCfg.Cabinet.Enabled = true
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true,
		BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	_ = fs.SetUserInfo(context.Background(), uinvAuthor, "", "Автор")
	return a, fm, fs
}

// Регистрация по почте с действующим ?inv= сразу даёт Доступ: без очереди на
// одобрение, Приглашение списано, Автор — Пригласивший. Бонус ждёт первой
// оплаты, как у любого пришедшего по почте.
func TestCabinetRegisterInvite_GrantsAccess(t *testing.T) {
	a, fm, fs := cabInvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c1", uinvAuthor)

	id, err := a.CabinetEmailRegister(ctx, "friend@example.com", "password1", "inv_c1")
	if err != nil {
		t.Fatalf("регистрация с приглашением: %v", err)
	}
	u, _ := fs.GetUser(ctx, id)
	if u == nil || !u.Whitelisted || u.ReferredBy != uinvAuthor || !u.RefOnPay {
		t.Fatalf("аккаунт после регистрации: %+v", u)
	}
	if a.MiniAccessDenied(ctx, id) || a.CabinetGate(ctx, id, true) != nil {
		t.Fatal("аккаунт с Доступом не пускают в кабинет")
	}
	if inviteUsed(t, fs, "c1") != 1 {
		t.Fatal("активация не списана")
	}
	if au, _ := fs.GetUser(ctx, uinvAuthor); au.Balance != 0 {
		t.Fatalf("бонус за адрес выдан до оплаты: %d коп.", au.Balance)
	}
	if !strings.Contains(fm.joined(), "По вашему приглашению пришёл") {
		t.Fatalf("Автор не уведомлён: %s", fm.joined())
	}
	// Вход по паролю потом пускает без одобрения.
	if _, err := a.CabinetEmailLogin(ctx, "friend@example.com", "password1"); err != nil {
		t.Fatalf("вход: %v", err)
	}
	// Человек с Доступом может приглашать сам.
	if !a.MiniInvites(ctx, id).Allowed {
		t.Fatal("почтовому аккаунту с Доступом не дают приглашать")
	}
}

// Без кода, с мёртвым и с приостановленным кодом — очередь на одобрение, как
// раньше; приостановленное Приглашение не тратится.
func TestCabinetRegisterInvite_NoOrDeadCodeQueues(t *testing.T) {
	a, _, fs := cabInvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "paused", uinvAuthor)

	for i, start := range []string{"", "inv_nosuch", "inv_paused"} {
		if start == "inv_paused" {
			a.botCfg.UserInvites = false
		}
		email := []string{"a@example.com", "b@example.com", "c@example.com"}[i]
		_, err := a.CabinetEmailRegister(ctx, email, "password1", start)
		if err == nil || !strings.Contains(err.Error(), "ожидает одобрения") {
			t.Fatalf("%q: не в очереди на одобрение: %v", start, err)
		}
		wu, _ := fs.GetWebUserByEmail(ctx, email)
		if wu == nil || !a.MiniAccessDenied(ctx, wu.TgID) {
			t.Fatalf("%q: аккаунт без Доступа пустили", start)
		}
	}
	if inviteUsed(t, fs, "paused") != 0 {
		t.Fatal("приостановленное приглашение потратилось")
	}
}

// Ждущий одобрения почтовый аккаунт, повторно открывший регистрацию по ссылке
// с приглашением, получает Доступ так же, как /start inv_ в чате.
func TestCabinetRegisterInvite_ExistingPending(t *testing.T) {
	a, _, fs := cabInvApp(t)
	ctx := context.Background()
	if _, err := a.CabinetEmailRegister(ctx, "late@example.com", "password1", ""); err == nil {
		t.Fatal("без кода обязан ждать одобрения")
	}
	userInvite(t, fs, "c1", uinvAuthor)
	if _, err := a.CabinetEmailRegister(ctx, "late@example.com", "wrong-pass", "inv_c1"); err == nil {
		t.Fatal("чужой пароль пустил")
	}
	if inviteUsed(t, fs, "c1") != 0 {
		t.Fatal("приглашение потрачено без пароля")
	}
	id, err := a.CabinetEmailRegister(ctx, "late@example.com", "password1", "inv_c1")
	if err != nil {
		t.Fatalf("с приглашением: %v", err)
	}
	if u, _ := fs.GetUser(ctx, id); u == nil || !u.Whitelisted || inviteUsed(t, fs, "c1") != 1 {
		t.Fatalf("Доступ не выдан: %+v", u)
	}
}

// Вход в кабинет через Telegram с ?inv= активирует Приглашение.
func TestCabinetTelegramInvite(t *testing.T) {
	a, _, fs := cabInvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c1", uinvAuthor)
	a.MiniStart(ctx, uinvFriend, "inv_c1", true)
	a.CabinetEnsureUser(ctx, uinvFriend)
	u, _ := fs.GetUser(ctx, uinvFriend)
	if u == nil || !u.Whitelisted || u.ReferredBy != uinvAuthor {
		t.Fatalf("друг после входа: %+v", u)
	}
	if a.MiniAccessDenied(ctx, uinvFriend) || a.CabinetGate(ctx, uinvFriend, false) != nil {
		t.Fatal("вход через Telegram с приглашением не пустил")
	}
}

// Привязка Telegram к почтовому аккаунту с Доступом: Доступ и Пригласивший
// остаются, хотя сам Telegram Доступа не имел; розданные аккаунтом
// Приглашения переезжают вместе с ним.
func TestCabinetInvite_BindKeepsAccess(t *testing.T) {
	a, _, fs := cabInvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c1", uinvAuthor)
	id, err := a.CabinetEmailRegister(ctx, "bind@example.com", "password1", "inv_c1")
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}
	mine := a.MiniInviteCreate(ctx, id)
	if !mine.OK {
		t.Fatalf("приглашение почтового аккаунта: %+v", mine)
	}

	const tg = int64(1000000801)
	if _, err := a.CabinetBindTelegram(ctx, id, tg); err != nil {
		t.Fatalf("привязка: %v", err)
	}
	u, _ := fs.GetUser(ctx, tg)
	if u == nil || !u.Whitelisted || u.ReferredBy != uinvAuthor || a.MiniAccessDenied(ctx, tg) {
		t.Fatalf("после привязки: %+v", u)
	}
	list, _ := fs.ListInvitesBy(ctx, tg)
	if len(list) != 1 {
		t.Fatalf("приглашения аккаунта не переехали: %+v", list)
	}
	// Друг по такому приглашению проходит: Автор не «пропал».
	const friend = int64(1000000802)
	a.MiniStart(ctx, friend, "inv_"+list[0].Code, false)
	if f, _ := fs.GetUser(ctx, friend); f == nil || !f.Whitelisted || f.ReferredBy != tg {
		t.Fatalf("приглашение после переезда: %+v", f)
	}
}

// Одобренный владельцем почтовый аккаунт в закрытом боте — тоже Доступ:
// привязка Telegram его сохраняет, а чужое Приглашение не тратится.
func TestCabinetApproved_IsAccess(t *testing.T) {
	a, _, fs := cabInvApp(t)
	ctx := context.Background()
	if _, err := a.CabinetEmailRegister(ctx, "ok@example.com", "password1", ""); err == nil {
		t.Fatal("без кода обязан ждать одобрения")
	}
	wu, _ := fs.GetWebUserByEmail(ctx, "ok@example.com")
	_ = fs.SetWebApproved(ctx, wu.TgID, true)
	userInvite(t, fs, "c1", uinvAuthor)
	if _, err := a.CabinetEmailRegister(ctx, "ok@example.com", "password1", "inv_c1"); err != nil {
		t.Fatalf("одобренный: %v", err)
	}
	if inviteUsed(t, fs, "c1") != 0 {
		t.Fatal("Доступ уже был, а приглашение потратилось")
	}
	const tg = int64(1000000803)
	if _, err := a.CabinetBindTelegram(ctx, wu.TgID, tg); err != nil {
		t.Fatalf("привязка одобренного: %v", err)
	}
	if a.MiniAccessDenied(ctx, tg) {
		t.Fatal("Доступ потерян при привязке")
	}
}

// Отклонённый владельцем почтовый аккаунт Доступа не имеет, даже если когда-то
// пришёл по приглашению.
func TestCabinetDenied_NoAccess(t *testing.T) {
	a, _, fs := cabInvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c1", uinvAuthor)
	id, err := a.CabinetEmailRegister(ctx, "x@example.com", "password1", "inv_c1")
	if err != nil {
		t.Fatalf("регистрация: %v", err)
	}
	_ = fs.SetWebDenied(ctx, id, true)
	if !a.MiniAccessDenied(ctx, id) || a.CabinetGate(ctx, id, true) == nil || a.MiniInvites(ctx, id).Allowed {
		t.Fatal("отклонённый аккаунт пускают")
	}
	if _, err := a.CabinetBindTelegram(ctx, id, 1000000804); err == nil {
		t.Fatal("отклонённый привязал Telegram без Доступа")
	}
}
