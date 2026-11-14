package app

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/i18n"
	"remnabot/internal/model"
)

const (
	uinvAuthor = int64(1000000501)
	uinvFriend = int64(1000000601)
)

// uinvApp — закрытый бот «по приглашениям» с включёнными Приглашениями от
// пользователей и Автором, у которого есть Доступ.
func uinvApp(t *testing.T) (*App, *fakeMsg, *fakeStore) {
	t.Helper()
	a, fm, fs := newTestApp(t)
	a.store = fs
	a.botCfg = &model.BotConfig{Installed: true, Language: "ru"}
	a.botUser = "testbot"
	ctx := context.Background()
	a.setAccessMode(ctx, model.AccessInvite, false)
	a.botCfg.UserInvites = true
	_ = fs.UpsertUser(ctx, uinvAuthor)
	_ = fs.SetWhitelisted(ctx, uinvAuthor, true)
	return a, fm, fs
}

func sentCallback(fm *fakeMsg, data string) bool {
	for _, d := range fm.allCallbackData() {
		if d == data {
			return true
		}
	}
	return false
}

func activeInvitesOf(t *testing.T, fs *fakeStore, author int64) []model.Invite {
	t.Helper()
	list, err := fs.ListInvitesBy(context.Background(), author)
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Invite
	now := time.Now().UTC()
	for _, inv := range list {
		if inv.Active(now) {
			out = append(out, inv)
		}
	}
	return out
}

// Тумблер виден только в режиме «По приглашениям», по умолчанию выключен и
// сохраняется в конфиге.
func TestUserInvites_OwnerToggle(t *testing.T) {
	a, fm, fs := newTestApp(t)
	a.store = fs
	a.botCfg = &model.BotConfig{Installed: true, Language: "ru"}
	ctx := context.Background()

	a.handleCallback(ctx, cb(100, "menu:access"))
	if sentCallback(fm, "acc:uinv") {
		t.Fatal("тумблер показан в публичном режиме")
	}
	a.setAccessMode(ctx, model.AccessInvite, false)
	a.handleCallback(ctx, cb(100, "menu:access"))
	if !sentCallback(fm, "acc:uinv") || !strings.Contains(strings.Join(fm.btnText, "|"), "❌ Приглашают пользователи") {
		t.Fatalf("нет выключенного тумблера: %v", fm.btnText)
	}
	a.handleCallback(ctx, cb(100, "acc:uinv"))
	if fs.cfg == nil || !fs.cfg.UserInvites {
		t.Fatal("включённый тумблер не сохранён в конфиге")
	}
	if !strings.Contains(strings.Join(fm.btnText, "|"), "✅ Приглашают пользователи") {
		t.Fatalf("тумблер не показан включённым: %v", fm.btnText)
	}
	// Чужое нажатие тумблер не трогает.
	a.handleCallback(ctx, cb(uinvAuthor, "acc:uinv"))
	if !fs.cfg.UserInvites {
		t.Fatal("тумблер переключил не владелец")
	}
	a.setAccessMode(ctx, model.AccessWhitelist, false)
	before := len(fm.allCallbackData())
	a.handleCallback(ctx, cb(100, "menu:access"))
	for _, d := range fm.allCallbackData()[before:] {
		if d == "acc:uinv" {
			t.Fatal("тумблер показан в режиме белого списка")
		}
	}
}

// С тумблером в главном меню вместо рефералки — «Пригласить друга»; без
// тумблера — как в chill.
func TestUserInvites_MenuButton(t *testing.T) {
	a, fm, _ := uinvApp(t)
	ctx := context.Background()
	a.botCfg.NormalizeReferral()
	a.botCfg.Referral.Enabled = true

	a.handleMessage(ctx, msgText(uinvAuthor, "/start"))
	if !sentCallback(fm, "menu:uinv") || sentCallback(fm, "menu:ref") {
		t.Fatalf("в меню нет «Пригласить друга» или осталась рефералка: %v", fm.allCallbackData())
	}
	a.botCfg.UserInvites = false
	before := len(fm.allCallbackData())
	a.handleMessage(ctx, msgText(uinvAuthor, "/start"))
	after := strings.Join(fm.allCallbackData()[before:], " ")
	if strings.Contains(after, "menu:uinv") || !strings.Contains(after, "menu:ref") {
		t.Fatalf("тумблер выключен, а меню не как в chill: %s", after)
	}
}

// Экран: создание даёт Приглашение на одну активацию и 30 суток, кнопку
// «Поделиться» с обеими ссылками и «Отозвать»; ref_-ссылки нет.
func TestUserInvites_CreateShareRevoke(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.NormalizeReferral()
	a.botCfg.Referral.Enabled = true
	a.botCfg.Cabinet.Enabled = true
	a.botCfg.Webhook.PublicBaseURL = "https://lk.example.org"

	a.handleCallback(ctx, cb(uinvAuthor, "menu:uinv"))
	if !sentCallback(fm, "uinv:new") {
		t.Fatalf("нет кнопки создания: %v", fm.allCallbackData())
	}
	a.handleCallback(ctx, cb(uinvAuthor, "uinv:new"))
	list := activeInvitesOf(t, fs, uinvAuthor)
	if len(list) != 1 {
		t.Fatalf("активных приглашений %d, ожидалось 1", len(list))
	}
	inv := list[0]
	exp, _ := time.Parse(time.RFC3339, inv.ExpiresAt)
	if inv.MaxUses != 1 || exp.Sub(time.Now()) < 29*24*time.Hour || exp.Sub(time.Now()) > 30*24*time.Hour+time.Minute {
		t.Fatalf("параметры приглашения: %+v", inv)
	}
	screen := fm.last()
	if strings.Contains(screen, "ref_") || !strings.Contains(screen, "inv_"+inv.Code) {
		t.Fatalf("экран приглашений: %s", screen)
	}
	tgLink := "https://t.me/testbot?start=inv_" + inv.Code
	cabLink := "https://lk.example.org/cabinet/?inv=" + inv.Code
	var share string
	for _, u := range fm.urls {
		if strings.HasPrefix(u, "https://t.me/share/url?") {
			share = u
		}
	}
	q, err := url.Parse(share)
	if err != nil || share == "" {
		t.Fatalf("нет кнопки «Поделиться»: %v", fm.urls)
	}
	if q.Query().Get("url") != tgLink || !strings.Contains(q.Query().Get("text"), cabLink) {
		t.Fatalf("ссылки в «Поделиться»: url=%q text=%q", q.Query().Get("url"), q.Query().Get("text"))
	}
	if !sentCallback(fm, "uinv:revoke:"+inv.Code) {
		t.Fatal("нет кнопки отзыва")
	}

	// Чужой отзыв не проходит, свой — да.
	_ = fs.UpsertUser(ctx, 1000000502)
	_ = fs.SetWhitelisted(ctx, 1000000502, true)
	a.handleCallback(ctx, cb(1000000502, "uinv:revoke:"+inv.Code))
	if len(activeInvitesOf(t, fs, uinvAuthor)) != 1 {
		t.Fatal("чужое приглашение отозвано")
	}
	a.handleCallback(ctx, cb(uinvAuthor, "uinv:revoke:"+inv.Code))
	if len(activeInvitesOf(t, fs, uinvAuthor)) != 0 {
		t.Fatal("своё приглашение не отозвано")
	}
}

// Не больше 3 активных и 15 созданий за скользящие сутки.
func TestUserInvites_Limits(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		a.handleCallback(ctx, cb(uinvAuthor, "uinv:new"))
	}
	if n := len(activeInvitesOf(t, fs, uinvAuthor)); n != 3 {
		t.Fatalf("активных %d, ожидалось 3", n)
	}
	if !strings.Contains(fm.last(), "3 активных") {
		t.Fatalf("нет отказа про активные: %s", fm.last())
	}

	// 15 созданий за сутки: отзыв не обходит лимит.
	b, fm2, fs2 := uinvApp(t)
	old := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	_ = fs2.CreateInvite(ctx, &model.Invite{Code: "old", MaxUses: 1, CreatedBy: uinvAuthor, CreatedAt: old, Revoked: true})
	for i := 0; i < 14; i++ {
		recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
		_ = fs2.CreateInvite(ctx, &model.Invite{Code: "r" + itoa(i), MaxUses: 1, CreatedBy: uinvAuthor, CreatedAt: recent, Revoked: true})
	}
	b.handleCallback(ctx, cb(uinvAuthor, "uinv:new"))
	if n := len(activeInvitesOf(t, fs2, uinvAuthor)); n != 1 {
		t.Fatalf("15-е создание за сутки не прошло: %d", n)
	}
	b.handleCallback(ctx, cb(uinvAuthor, "uinv:new"))
	if n := len(activeInvitesOf(t, fs2, uinvAuthor)); n != 1 {
		t.Fatalf("16-е создание за сутки прошло: %d", n)
	}
	if !strings.Contains(fm2.last(), "15") {
		t.Fatalf("нет отказа про сутки: %s", fm2.last())
	}
}

// Двойное нажатие «Создать» при двух активных создаёт только одно.
func TestUserInvites_DoubleCreate(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	for _, c := range []string{"a1", "a2"} {
		_ = fs.CreateInvite(ctx, &model.Invite{Code: c, MaxUses: 1, CreatedBy: uinvAuthor,
			ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)})
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.createUserInvite(ctx, uinvAuthor)
		}()
	}
	wg.Wait()
	if n := len(activeInvitesOf(t, fs, uinvAuthor)); n != 3 {
		t.Fatalf("активных %d, ожидалось 3", n)
	}
}

// Без Доступа и заблокированный экран не открывают и не создают.
func TestUserInvites_NoRight(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	_ = fs.SetBlocked(ctx, uinvAuthor, true)
	a.handleCallback(ctx, cb(uinvAuthor, "uinv:new"))
	if _, reason := a.createUserInvite(ctx, uinvAuthor); reason == "" {
		t.Fatal("заблокированный создал приглашение")
	}
	_ = fs.SetBlocked(ctx, uinvAuthor, false)
	a.botCfg.UserInvites = false
	if _, reason := a.createUserInvite(ctx, uinvAuthor); reason == "" {
		t.Fatal("приглашение создано при выключенном тумблере")
	}
	if n := len(activeInvitesOf(t, fs, uinvAuthor)); n != 0 {
		t.Fatalf("создано %d приглашений без права", n)
	}
}

func startMsg(uid int64, firstName, payload string) *models.Message {
	m := msgText(uid, "/start "+payload)
	m.From.FirstName = firstName
	return m
}

func userInvite(t *testing.T, fs *fakeStore, code string, author int64) {
	t.Helper()
	if err := fs.CreateInvite(context.Background(), &model.Invite{Code: code, MaxUses: 1, CreatedBy: author,
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
}

func inviteUsed(t *testing.T, fs *fakeStore, code string) int {
	t.Helper()
	inv, _ := fs.GetInvite(context.Background(), code)
	if inv == nil {
		t.Fatalf("нет приглашения %s", code)
	}
	return inv.Used
}

// Активация: Доступ, Автор — Пригласивший, бонус по настройкам рефералки,
// уведомления Автору и владельцу.
func TestUserInvites_Activation(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, OnFirstPay: false,
		BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	_ = fs.SetUserInfo(ctx, uinvAuthor, "", "Автор")
	userInvite(t, fs, "c1", uinvAuthor)

	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c1"))
	u, _ := fs.GetUser(ctx, uinvFriend)
	if u == nil || !u.Whitelisted || u.ReferredBy != uinvAuthor {
		t.Fatalf("друг после активации: %+v", u)
	}
	if inviteUsed(t, fs, "c1") != 1 {
		t.Fatal("активация не списана")
	}
	if au, _ := fs.GetUser(ctx, uinvAuthor); au.Balance != 5000 {
		t.Fatalf("бонус Автору: %d коп., ожидалось 5000", au.Balance)
	}
	all := fm.joined()
	if !strings.Contains(all, "По вашему приглашению пришёл Друг\n") ||
		!strings.Contains(all, "Автор (1000000501) пригласил Друг (1000000601)") {
		t.Fatalf("нет уведомлений об активации: %s", all)
	}
}

// Админское Приглашение никого не делает Пригласившим.
func TestUserInvites_AdminInviteNoReferrer(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	_ = fs.CreateInvite(ctx, &model.Invite{Code: "adm", MaxUses: 1})
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_adm"))
	if u, _ := fs.GetUser(ctx, uinvFriend); u == nil || !u.Whitelisted || u.ReferredBy != 0 {
		t.Fatalf("друг по админскому приглашению: %+v", u)
	}
}

// Истёкшее, использованное, отозванное и приостановленное Приглашение
// отказывают текстом «попросите у друга новое»; приостановленное не тратится.
func TestUserInvites_Invalid(t *testing.T) {
	cases := []struct {
		name  string
		setup func(a *App, fs *fakeStore)
	}{
		{"истёкшее", func(_ *App, fs *fakeStore) {
			fs.invites["c"].ExpiresAt = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
		}},
		{"использованное", func(_ *App, fs *fakeStore) { fs.invites["c"].Used = 1 }},
		{"отозванное", func(_ *App, fs *fakeStore) { fs.invites["c"].Revoked = true }},
		{"тумблер выключен", func(a *App, _ *fakeStore) { a.botCfg.UserInvites = false }},
		{"Автор заблокирован", func(_ *App, fs *fakeStore) { _ = fs.SetBlocked(context.Background(), uinvAuthor, true) }},
		{"Автор без Доступа", func(_ *App, fs *fakeStore) { _ = fs.SetWhitelisted(context.Background(), uinvAuthor, false) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, fm, fs := uinvApp(t)
			ctx := context.Background()
			userInvite(t, fs, "c", uinvAuthor)
			c.setup(a, fs)
			used := inviteUsed(t, fs, "c")
			a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c"))
			if !strings.Contains(fm.joined(), "Приглашение недействительно — попросите у друга новое") {
				t.Fatalf("нет отказа: %s", fm.joined())
			}
			if u, _ := fs.GetUser(ctx, uinvFriend); u != nil && u.Whitelisted {
				t.Fatal("Доступ выдан по недействительному приглашению")
			}
			if inviteUsed(t, fs, "c") != used {
				t.Fatal("недействительное приглашение потрачено")
			}
		})
	}
}

// Пауза снимается: Автору вернули Доступ — его неистёкшее Приглашение снова
// работает.
func TestUserInvites_PauseLifted(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c", uinvAuthor)
	_ = fs.SetBlocked(ctx, uinvAuthor, true)
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c"))
	_ = fs.SetBlocked(ctx, uinvAuthor, false)
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c"))
	if u, _ := fs.GetUser(ctx, uinvFriend); u == nil || !u.Whitelisted {
		t.Fatalf("после снятия паузы приглашение не сработало: %+v", u)
	}
}

// У кого Доступ уже есть, код не тратится и Пригласивший не меняется; два
// кода подряд — второй не тратится.
func TestUserInvites_AlreadyHasAccess(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	const other = int64(1000000502)
	_ = fs.UpsertUser(ctx, other)
	_ = fs.SetWhitelisted(ctx, other, true)
	userInvite(t, fs, "c1", uinvAuthor)
	userInvite(t, fs, "c2", other)

	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c1"))
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c2"))
	if inviteUsed(t, fs, "c2") != 0 {
		t.Fatal("второй код потрачен")
	}
	if u, _ := fs.GetUser(ctx, uinvFriend); u.ReferredBy != uinvAuthor {
		t.Fatalf("Пригласивший сменился: %d", u.ReferredBy)
	}
}

// Два кода одним человеком одновременно: тратится ровно один.
func TestUserInvites_TwoCodesConcurrently(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c1", uinvAuthor)
	userInvite(t, fs, "c2", uinvAuthor)
	var wg sync.WaitGroup
	for _, c := range []string{"c1", "c2"} {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			a.redeemInvite(ctx, uinvFriend, code, "", "")
		}(c)
	}
	wg.Wait()
	if n := inviteUsed(t, fs, "c1") + inviteUsed(t, fs, "c2"); n != 1 {
		t.Fatalf("потрачено активаций: %d, ожидалась 1", n)
	}
}

// Заблокированный не тратит Приглашение и получает сообщение о блокировке.
func TestUserInvites_BlockedFriend(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c", uinvAuthor)
	_ = fs.SetBlocked(ctx, uinvFriend, true)
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c"))
	if inviteUsed(t, fs, "c") != 0 {
		t.Fatal("заблокированный потратил приглашение")
	}
	if !strings.Contains(fm.last(), i18n.T("ru", "user.you_blocked")) {
		t.Fatalf("нет сообщения о блокировке: %s", fm.last())
	}
}

// Без Доступа с тумблером — отказ «нужно приглашение от друга»; ref_ не
// пускает и рефералка в чате ref_-ссылку не показывает.
func TestUserInvites_DenyAndNoRefLink(t *testing.T) {
	a, fm, fs := uinvApp(t)
	ctx := context.Background()
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50}
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "ref_"+itoa(int(uinvAuthor))))
	if !strings.Contains(fm.last(), "только по приглашению от друга") {
		t.Fatalf("нет отказа: %s", fm.last())
	}
	if u, _ := fs.GetUser(ctx, uinvFriend); u != nil {
		t.Fatalf("ref_ завёл строку: %+v", u)
	}
	a.handleCallback(ctx, cb(uinvAuthor, "menu:ref"))
	if strings.Contains(fm.last(), "ref_") || !strings.Contains(fm.last(), "Пригласить друга") {
		t.Fatalf("рефералка показала ref_: %s", fm.last())
	}
}

// Автор отзывает своё Приглашение и на паузе: выключенный тумблер не держит
// утёкшую ссылку живой до повторного включения.
func TestUserInvites_RevokeWhilePaused(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c", uinvAuthor)
	a.botCfg.UserInvites = false
	a.handleCallback(ctx, cb(uinvAuthor, "uinv:revoke:c"))
	if inv, _ := fs.GetInvite(ctx, "c"); !inv.Revoked {
		t.Fatal("приглашение на паузе не отозвано")
	}
}

// Рефералка выключена: друг получает Доступ, а Пригласивший не привязывается —
// как при ref_ в chill.
func TestUserInvites_ReferralOff(t *testing.T) {
	a, _, fs := uinvApp(t)
	ctx := context.Background()
	userInvite(t, fs, "c", uinvAuthor)
	a.handleMessage(ctx, startMsg(uinvFriend, "Друг", "inv_c"))
	if u, _ := fs.GetUser(ctx, uinvFriend); u == nil || !u.Whitelisted || u.ReferredBy != 0 {
		t.Fatalf("друг при выключенной рефералке: %+v", u)
	}
}
