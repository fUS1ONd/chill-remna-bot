package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"remnabot/internal/model"
	"remnabot/internal/remnawave"
)

// Бессрочная подписка узнаётся по сроку: окончание не раньше 1 января 2090
// (UTC). Перенесённые «вечные» 2099 попадают под правило сами.
func TestPerpetualExpiry(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"2099-01-01T00:00:00Z", true},
		{"2090-01-01T00:00:00Z", true},
		{"2090-01-01T00:00:00.000Z", true},
		{"2089-12-31T23:59:59Z", false},
		{"2089-06-01T00:00:00Z", false},
		{"2026-11-14T10:00:00Z", false},
		{"", false},
		{"мусор", false},
	} {
		if got := perpetualExpiry(tc.raw); got != tc.want {
			t.Errorf("perpetualExpiry(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

const (
	perpetualUID  = int64(1000000007)
	perpetual2099 = "2099-01-01T00:00:00Z"
	// ordinary2089 — последний год перед границей: обычный платник.
	ordinary2089 = "2089-12-31T23:00:00Z"
)

// perpetualApp — витрина с триалом, рефералкой и пополнением; в панели у
// человека подписка со сроком expire, в базе бота — тот же срок.
func perpetualApp(t *testing.T, expire string) (*App, *fakeMsg) {
	t.Helper()
	a, fm, _ := storefrontApp(t)
	ctx := context.Background()
	a.botCfg.Trial = model.TrialConfig{Enabled: true, Days: 3}
	a.botCfg.Referral = model.ReferralConfig{Enabled: true, Init: true, BonusKind: model.ReferralBonusBalance, BonusValue: 50, Percent: 10}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":[{"uuid":"u-1","username":"u1","status":"ACTIVE","expireAt":"` + expire +
			`","subscriptionUrl":"https://sub.example/x","telegramId":` + itoa64(perpetualUID) + `}]}`))
	}))
	t.Cleanup(srv.Close)
	a.panel = remnawave.New(model.PanelConfig{Mode: model.ModeRemote, BaseURL: srv.URL, APIToken: "t"})
	_ = a.store.UpsertUser(ctx, perpetualUID)
	_ = a.store.SetSubExpiry(ctx, perpetualUID, expire, "paid")
	return a, fm
}

func menuLabels(a *App, fm *fakeMsg, uid int64) string {
	n := len(fm.buttonLabels())
	a.handleCallback(context.Background(), cb(uid, "menu:home"))
	return strings.Join(fm.buttonLabels()[n:], "|")
}

// Бессрочному меню бота показывает подписку и «Пригласить друга», но не
// покупку, продление, триал и кошелёк; человек со сроком в 2089 — обычный.
func TestPerpetualBotMenu(t *testing.T) {
	a, fm := perpetualApp(t, perpetual2099)
	got := menuLabels(a, fm, perpetualUID)
	for _, want := range []string{"Мои подписки", "Пригласить"} {
		if !strings.Contains(got, want) {
			t.Errorf("в меню бессрочного нет %q: %s", want, got)
		}
	}
	for _, bad := range []string{"Купить", "Продлить", "Триал", "Кошел"} {
		if strings.Contains(got, bad) {
			t.Errorf("в меню бессрочного есть %q: %s", bad, got)
		}
	}

	a2, fm2 := perpetualApp(t, ordinary2089)
	if got := menuLabels(a2, fm2, perpetualUID); !strings.Contains(got, "Кошел") {
		t.Errorf("у срока 2089 пропал кошелёк: %s", got)
	}
}

// Кнопки из старых сообщений и /buy бессрочного возвращают в меню, а не
// открывают витрину, продление, триал, автопродление или кошелёк.
func TestPerpetualBotOldButtons(t *testing.T) {
	a, fm := perpetualApp(t, perpetual2099)
	ctx := context.Background()
	for _, data := range []string{"menu:buy", "menu:renew", "menu:trial", "menu:autopay", "menu:balance", "menu:topup", "plo:" + model.PlanCodeBase} {
		n, nb := len(fm.texts), len(fm.buttonLabels())
		a.handleCallback(ctx, cb(perpetualUID, data))
		out := strings.Join(fm.texts[n:], "\n") + "\n" + strings.Join(fm.buttonLabels()[nb:], "|")
		if !strings.Contains(out, "Мои подписки") || strings.Contains(out, "₽") {
			t.Errorf("%s: бессрочный не вернулся в меню:\n%s", data, out)
		}
		for _, bad := range []string{"Выберите тариф", "Баланс", "Автопродление", "Пробный", "триал"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: бессрочному показано %q:\n%s", data, bad, out)
			}
		}
	}
	n := len(fm.buttonLabels())
	a.handleMessage(ctx, msgText(perpetualUID, "/buy"))
	if got := strings.Join(fm.buttonLabels()[n:], "|"); strings.Contains(got, "₽") {
		t.Errorf("/buy открыл бессрочному витрину: %s", got)
	}
}

// «Мои подписки» бессрочного: «Бессрочная» вместо даты, подключение и
// устройства на месте, автопродления нет.
func TestPerpetualMySubs(t *testing.T) {
	a, fm := perpetualApp(t, perpetual2099)
	n, nb := len(fm.texts), len(fm.buttonLabels())
	a.handleCallback(context.Background(), cb(perpetualUID, "menu:mysubs"))
	out := strings.Join(fm.texts[n:], "\n")
	btns := strings.Join(fm.buttonLabels()[nb:], "|")
	if !strings.Contains(out, "Бессрочная") || strings.Contains(out, "2099") {
		t.Errorf("вместо даты не «Бессрочная»:\n%s", out)
	}
	if !strings.Contains(out, "https://sub.example/x") || !strings.Contains(btns, "Сбросить") {
		t.Errorf("пропали подключение или устройства:\n%s\n%s", out, btns)
	}
	if strings.Contains(btns, "втопродл") || strings.Contains(btns, "Продлить") {
		t.Errorf("бессрочному предложено продление: %s", btns)
	}

	a2, fm2 := perpetualApp(t, ordinary2089)
	n = len(fm2.texts)
	a2.handleCallback(context.Background(), cb(perpetualUID, "menu:mysubs"))
	if out := strings.Join(fm2.texts[n:], "\n"); !strings.Contains(out, "31.12.2089") && !strings.Contains(out, "01.01.2090") {
		t.Errorf("у срока 2089 нет даты:\n%s", out)
	}
}

// Мини-апп и кабинет получают от сервера то же, что видит чат: бессрочному
// нет витрины, продления, триала, автопродления, пополнения и доп-подписки,
// а вместо даты — «Бессрочная».
func TestPerpetualMiniDTO(t *testing.T) {
	ctx := context.Background()
	a, _ := perpetualApp(t, perpetual2099)
	a.botCfg.YooKassa.Enabled = true

	m := a.MiniMenu(ctx, perpetualUID, false)
	if !m.HasSub || !m.Perpetual || m.CanRenew || m.TrialAvailable || m.TopUpOn || !m.ReferralOn || m.OwnPlan != "" || len(m.PayMethods) != 0 {
		t.Errorf("меню бессрочного: %+v", m)
	}
	s := a.MiniSubscription(ctx, perpetualUID)
	if !s.Active || !s.Perpetual || s.ExpireAt != "Бессрочная" || s.ExpireTS != 0 || s.SubURL == "" || s.AddSubOK {
		t.Errorf("подписка бессрочного: %+v", s)
	}
	if p := a.MiniPlans(ctx, perpetualUID); len(p.Plans) != 0 {
		t.Errorf("бессрочному отдана витрина: %+v", p.Plans)
	}
	if p := a.MiniPlanLink(ctx, perpetualUID, model.PlanCodeBase); len(p.Plans) != 0 {
		t.Errorf("бессрочному отдан тариф по ссылке: %+v", p.Plans)
	}
	if ap := a.MiniAutoPay(ctx, perpetualUID); ap.Available || ap.On || ap.CanEnable {
		t.Errorf("бессрочному отдано автопродление: %+v", ap)
	}
	if w := a.MiniWallet(ctx, perpetualUID); w.TopUpOn {
		t.Errorf("бессрочному открыто пополнение: %+v", w)
	}
	if o := a.MiniTopUpOptions(ctx, perpetualUID); len(o.Amounts) != 0 {
		t.Errorf("бессрочному отданы суммы пополнения: %+v", o)
	}
	for name, r := range map[string]func() bool{
		"триал": func() bool { return a.MiniTrial(ctx, perpetualUID).OK },
		"покупка": func() bool {
			return a.MiniCheckout(ctx, perpetualUID, model.PlanCodeBase, 1, model.PayMethodYooKassa, "", false, false).OK
		},
		"пополнение":    func() bool { return a.MiniTopUp(ctx, perpetualUID, 10000, model.PayMethodYooKassa, false).OK },
		"автопродление": func() bool { return a.MiniSetAutoPay(ctx, perpetualUID, true).OK },
	} {
		if r() {
			t.Errorf("бессрочному удалось: %s", name)
		}
	}

	a2, _ := perpetualApp(t, ordinary2089)
	if m := a2.MiniMenu(ctx, perpetualUID, false); m.Perpetual || !m.TopUpOn {
		t.Errorf("меню у срока 2089: %+v", m)
	}
	if s := a2.MiniSubscription(ctx, perpetualUID); s.Perpetual || s.ExpireTS == 0 {
		t.Errorf("подписка у срока 2089: %+v", s)
	}
	if p := a2.MiniPlans(ctx, perpetualUID); len(p.Plans) == 0 {
		t.Errorf("у срока 2089 пустая витрина")
	}
}

// Бессрочному Пригласившему рефералка ничего не начисляет — ни бонус, ни
// процент — и помечает выплату сделанной; друг свой бонус получает.
func TestPerpetualReferrer(t *testing.T) {
	ctx := context.Background()
	const friend = int64(1000000099)
	for _, tc := range []struct {
		expire  string
		refBal  int64
		percent int64
	}{
		{perpetual2099, 0, 0},
		{ordinary2089, 5000, 10000},
	} {
		a, _ := perpetualApp(t, tc.expire)
		a.botCfg.Referral.InviteeKind = model.ReferralBonusBalance
		a.botCfg.Referral.InviteeValue = 30
		_ = a.store.UpsertUser(ctx, friend)
		_ = a.store.SetReferredBy(ctx, friend, perpetualUID)

		a.payReferralBonus(ctx, friend, true)
		u, _ := a.store.GetUser(ctx, friend)
		if u == nil || !u.RefBonusPaid || u.Balance != 3000 {
			t.Errorf("%s: друг без бонуса или выплата не помечена: %+v", tc.expire, u)
		}
		if r, _ := a.store.GetUser(ctx, perpetualUID); r.Balance != tc.refBal || r.RefEarned != tc.refBal {
			t.Errorf("%s: бонус Пригласившему %d/%d, want %d", tc.expire, r.Balance, r.RefEarned, tc.refBal)
		}
		a.creditReferralPercent(ctx, friend, "1000 ₽")
		if r, _ := a.store.GetUser(ctx, perpetualUID); r.Balance-tc.refBal != tc.percent {
			t.Errorf("%s: процент Пригласившему %d, want %d", tc.expire, r.Balance-tc.refBal, tc.percent)
		}
	}
}

// Напоминания об окончании бессрочному не уходят — в том числе когда в базе
// бота ещё старый близкий срок, а владелец уже сдвинул его в панели за 2090.
func TestPerpetualNoReminders(t *testing.T) {
	ctx := context.Background()
	soon := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	for _, local := range []string{perpetual2099, soon} {
		a, fm := perpetualApp(t, perpetual2099)
		a.botCfg.Reminders = model.RemindersConfig{Enabled: true, DaysList: []int{3, 1}, Init: true}
		_ = a.store.SetSubExpiry(ctx, perpetualUID, local, "paid")
		a.remindOnce(ctx)
		if out := strings.Join(fm.texts, "\n"); strings.Contains(out, "заканчивается") {
			t.Errorf("локальный срок %s: бессрочному ушло напоминание:\n%s", local, out)
		}
	}
	// Тот же близкий срок у обычного платника напоминание даёт.
	a, fm := perpetualApp(t, soon)
	a.botCfg.Reminders = model.RemindersConfig{Enabled: true, DaysList: []int{3, 1}, Init: true}
	a.remindOnce(ctx)
	if !strings.Contains(strings.Join(fm.texts, "\n"), "заканчивается") {
		t.Errorf("обычному платнику напоминание не ушло:\n%s", strings.Join(fm.texts, "\n"))
	}
}
