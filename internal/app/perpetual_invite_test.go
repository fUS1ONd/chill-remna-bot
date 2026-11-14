package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"remnabot/internal/model"
	"remnabot/internal/remnawave"
)

const (
	pinvFriend  = int64(1000000701)
	pinvFriend2 = int64(1000000702)
	pinvFriend3 = int64(1000000703)
)

// permPanel — фейковая панель с учётками по telegram id: запоминает тела
// созданий и правок, умеет отказывать на запись.
type permPanel struct {
	mu      sync.Mutex
	users   map[int64]map[string]any
	posts   []map[string]any
	patches []map[string]any
	failW   bool
}

func (p *permPanel) seed(tgID int64, expireAt string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users[tgID] = map[string]any{
		"uuid": "u-" + strconv.FormatInt(tgID, 10), "username": "tg_" + strconv.FormatInt(tgID, 10),
		"telegramId": tgID, "tag": remnawave.BotTag, "expireAt": expireAt,
		"subscriptionUrl": "https://sub/" + strconv.FormatInt(tgID, 10),
	}
}

func (p *permPanel) writes() (posts, patches []map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.posts...), append([]map[string]any(nil), p.patches...)
}

func newPermPanel(t *testing.T) (*permPanel, *httptest.Server) {
	t.Helper()
	p := &permPanel{users: map[int64]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case strings.Contains(path, "/by-telegram-id/"):
			id, _ := strconv.ParseInt(path[strings.LastIndex(path, "/")+1:], 10, 64)
			list := []map[string]any{}
			if u := p.users[id]; u != nil {
				list = append(list, u)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": list})
		case strings.HasSuffix(path, "/internal-squads"):
			_, _ = w.Write([]byte(`{"response":{"internalSquads":[{"uuid":"sq-base","name":"Base"},{"uuid":"sq-custom","name":"Custom"}]}}`))
		case strings.HasSuffix(path, "/external-squads"):
			_, _ = w.Write([]byte(`{"response":{"externalSquads":[{"uuid":"ext-base","name":"Ext"}]}}`))
		case path == "/api/users" && (r.Method == http.MethodPost || r.Method == http.MethodPatch):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if p.failW {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
				return
			}
			var u map[string]any
			if r.Method == http.MethodPost {
				p.posts = append(p.posts, body)
				id := int64(body["telegramId"].(float64))
				p.seedLocked(id, body)
				u = p.users[id]
			} else {
				p.patches = append(p.patches, body)
				for _, cand := range p.users {
					if cand["uuid"] == body["uuid"] {
						for k, v := range body {
							cand[k] = v
						}
						u = cand
					}
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"response": u})
		default:
			_, _ = w.Write([]byte(`{"response":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

func (p *permPanel) seedLocked(id int64, body map[string]any) {
	u := map[string]any{"uuid": "u-" + strconv.FormatInt(id, 10), "subscriptionUrl": "https://sub/" + strconv.FormatInt(id, 10)}
	for k, v := range body {
		u[k] = v
	}
	p.users[id] = u
}

// pinvApp — закрытый бот «по приглашениям», «Базовый» со своими сквадами,
// лимитом устройств и стратегией, панель — фейк.
func pinvApp(t *testing.T) (*App, *fakeMsg, *fakeStore, *permPanel) {
	t.Helper()
	a, fm, fs := newTestApp(t)
	a.store = fs
	a.botCfg = &model.BotConfig{Installed: true, Language: "ru"}
	a.botCfg.Plan.ActiveInternalSquads = []string{"sq-base"}
	a.botCfg.Plan.ExternalSquadUUID = "ext-base"
	a.botCfg.Pricing = model.Pricing{Currency: "₽", Base: map[int]string{1: "150"}, DeviceLimit: 4, TrafficStrategy: "WEEK",
		Traffic: map[int]int{1: 50}}
	a.botCfg.NormalizePricing()
	a.botUser = "testbot"
	a.setAccessMode(context.Background(), model.AccessInvite, false)
	pp, srv := newPermPanel(t)
	a.panel = testPanel(srv.URL)
	return a, fm, fs, pp
}

func perpetualInvite(t *testing.T, fs *fakeStore, code string, uses int) {
	t.Helper()
	if err := fs.CreateInvite(context.Background(), &model.Invite{Code: code, MaxUses: uses, Grant: model.InviteGrantPerpetual}); err != nil {
		t.Fatal(err)
	}
}

// Мастер владельца: кнопка «♾ Безлимит-приглашение», срок жизни, число
// активаций — Приглашение с выдачей Бессрочной; не владельцу мастер не
// открывается.
func TestPerpetualInvite_OwnerWizard(t *testing.T) {
	a, fm, fs, _ := pinvApp(t)
	ctx := context.Background()

	a.handleCallback(ctx, cb(100, "menu:access"))
	if !sentCallback(fm, "acc:newp") {
		t.Fatalf("на экране доступа нет «♾ Безлимит-приглашение»: %v", fm.allCallbackData())
	}
	a.handleCallback(ctx, cb(100, "acc:newp"))
	a.handleMessage(ctx, msgText(100, "7"))
	a.handleMessage(ctx, msgText(100, "3"))
	list, _ := fs.ListInvitesBy(ctx, 0)
	if len(list) != 1 {
		t.Fatalf("приглашений %d, ожидалось 1", len(list))
	}
	inv := list[0]
	if inv.Grant != model.InviteGrantPerpetual || inv.MaxUses != 3 || inv.CreatedBy != 0 {
		t.Fatalf("приглашение: %+v", inv)
	}
	exp, err := time.Parse(time.RFC3339, inv.ExpiresAt)
	if err != nil || time.Until(exp) < 6*24*time.Hour || time.Until(exp) > 8*24*time.Hour {
		t.Fatalf("срок жизни ссылки: %q", inv.ExpiresAt)
	}
	if !strings.Contains(fm.joined(), "♾") {
		t.Fatalf("карточка не помечает безлимитное: %s", fm.joined())
	}

	// Следом обычное приглашение того же мастера выдаёт только Доступ.
	a.handleCallback(ctx, cb(100, "acc:new"))
	a.handleMessage(ctx, msgText(100, "0"))
	a.handleMessage(ctx, msgText(100, "1"))
	list, _ = fs.ListInvitesBy(ctx, 0)
	plain := 0
	for _, i := range list {
		if i.Grant == model.InviteGrantAccess {
			plain++
		}
	}
	if len(list) != 2 || plain != 1 {
		t.Fatalf("обычное приглашение после безлимитного: %+v", list)
	}

	// Не владелец мастер не открывает.
	_ = fs.UpsertUser(ctx, pinvFriend)
	_ = fs.SetWhitelisted(ctx, pinvFriend, true)
	a.handleCallback(ctx, cb(pinvFriend, "acc:newp"))
	a.handleMessage(ctx, msgText(pinvFriend, "7"))
	a.handleMessage(ctx, msgText(pinvFriend, "3"))
	if list, _ = fs.ListInvitesBy(ctx, 0); len(list) != 2 {
		t.Fatalf("не владелец создал приглашение: %+v", list)
	}
}

// Карточка: пометка безлимитного и «Кто пришёл»; в списке — значок.
func TestPerpetualInvite_Card(t *testing.T) {
	a, fm, fs, _ := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "pc", 2)
	a.handleMessage(ctx, startMsg(pinvFriend, "Друг", "inv_pc"))

	before := len(fm.texts)
	a.handleCallback(ctx, cb(100, "acc:show:pc"))
	card := strings.Join(fm.texts[before:], "\n")
	if !strings.Contains(card, "♾ Безлимит-приглашение") {
		t.Fatalf("карточка не помечает безлимитное: %s", card)
	}
	if !sentCallback(fm, "acc:who:pc:0") {
		t.Fatalf("в карточке нет «Кто пришёл»: %v", fm.allCallbackData())
	}
	a.handleCallback(ctx, cb(100, "acc:list"))
	if !strings.Contains(strings.Join(fm.btnText, "|"), "♾") {
		t.Fatalf("в списке нет значка безлимитного: %v", fm.btnText)
	}
}

// Новичок без учётки: учётка создаётся со сроком 2099-12-31, без лимита
// трафика, со сквадами, лимитом устройств и стратегией «Базового».
func TestPerpetualInvite_NewAccount(t *testing.T) {
	a, fm, fs, pp := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "p1", 1)

	a.handleMessage(ctx, startMsg(pinvFriend, "Друг", "inv_p1"))

	if u, _ := fs.GetUser(ctx, pinvFriend); u == nil || !u.Whitelisted {
		t.Fatalf("Доступ не выдан: %+v", u)
	}
	posts, patches := pp.writes()
	if len(posts) != 1 || len(patches) != 0 {
		t.Fatalf("создания %d, правки %d", len(posts), len(patches))
	}
	b := posts[0]
	if !strings.HasPrefix(b["expireAt"].(string), "2099-12-31") {
		t.Fatalf("срок: %v", b["expireAt"])
	}
	if v, ok := b["trafficLimitBytes"]; !ok || v.(float64) != 0 {
		t.Fatalf("трафик не без лимита: %v", b)
	}
	sq, _ := b["activeInternalSquads"].([]any)
	if len(sq) != 1 || sq[0] != "sq-base" || b["externalSquadUuid"] != "ext-base" {
		t.Fatalf("сквады не «Базового»: %v", b)
	}
	if b["hwidDeviceLimit"] != float64(4) || b["trafficLimitStrategy"] != "WEEK" {
		t.Fatalf("устройства/стратегия не «Базового»: %v", b)
	}
	if !a.subPerpetual(ctx, pinvFriend) {
		t.Fatal("подписка не Бессрочная")
	}
	if !strings.Contains(fm.joined(), "Бессрочная подписка") {
		t.Fatalf("человеку не сказано о Бессрочной: %s", fm.joined())
	}
}

// Учётка есть (с чужими сквадами): меняются только срок и трафик.
func TestPerpetualInvite_ExistingAccountKeepsLimits(t *testing.T) {
	a, _, fs, pp := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "p1", 1)
	pp.seed(pinvFriend, "2027-01-01T00:00:00Z")

	a.handleMessage(ctx, startMsg(pinvFriend, "Друг", "inv_p1"))

	posts, patches := pp.writes()
	if len(posts) != 0 || len(patches) != 1 {
		t.Fatalf("создания %d, правки %d", len(posts), len(patches))
	}
	b := patches[0]
	if !strings.HasPrefix(b["expireAt"].(string), "2099-12-31") || b["trafficLimitBytes"] != float64(0) {
		t.Fatalf("срок/трафик: %v", b)
	}
	for _, k := range []string{"activeInternalSquads", "externalSquadUuid", "hwidDeviceLimit", "trafficLimitStrategy"} {
		if _, ok := b[k]; ok {
			t.Fatalf("правка тронула %s: %v", k, b)
		}
	}
	if u, _ := fs.GetUser(ctx, pinvFriend); u == nil || !perpetualExpiry(u.SubExpireAt) {
		t.Fatalf("срок в базе бота не записан: %+v", u)
	}
}

// Уже допущенный платник: приглашение списано, Бессрочная выдана, запись
// автопродления удалена целиком.
func TestPerpetualInvite_UpgradeAdmitted(t *testing.T) {
	a, fm, fs, pp := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "p1", 1)
	_ = fs.UpsertUser(ctx, pinvFriend)
	_ = fs.SetWhitelisted(ctx, pinvFriend, true)
	pp.seed(pinvFriend, "2027-01-01T00:00:00Z")
	_ = fs.SetAutoPay(ctx, &model.AutoPay{TelegramID: pinvFriend, Method: model.PayMethodYooKassa, MethodID: "pm-1", Months: 1, Enabled: true})

	a.handleMessage(ctx, startMsg(pinvFriend, "Друг", "inv_p1"))

	if inviteUsed(t, fs, "p1") != 1 {
		t.Fatal("приглашение не списано")
	}
	if ap, _ := fs.GetAutoPay(ctx, pinvFriend); ap != nil {
		t.Fatalf("запись автопродления осталась: %+v", ap)
	}
	if _, patches := pp.writes(); len(patches) != 1 {
		t.Fatalf("правок в панели %d", len(patches))
	}
	if !a.subPerpetual(ctx, pinvFriend) {
		t.Fatal("подписка не Бессрочная")
	}
	if !strings.Contains(fm.joined(), "Бессрочная подписка") {
		t.Fatalf("человеку не сказано о Бессрочной: %s", fm.joined())
	}
}

// Уже бессрочный: приглашение не тратится, панель не трогается.
func TestPerpetualInvite_AlreadyPerpetualNotSpent(t *testing.T) {
	a, _, fs, pp := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "p1", 1)
	_ = fs.UpsertUser(ctx, pinvFriend)
	_ = fs.SetWhitelisted(ctx, pinvFriend, true)
	pp.seed(pinvFriend, "2099-01-01T00:00:00Z")

	a.handleMessage(ctx, startMsg(pinvFriend, "Друг", "inv_p1"))

	if inviteUsed(t, fs, "p1") != 0 {
		t.Fatal("приглашение списано у бессрочного")
	}
	if posts, patches := pp.writes(); len(posts)+len(patches) != 0 {
		t.Fatal("панель тронута у бессрочного")
	}
}

// Сбой панели после выдачи Доступа: Доступ есть, человеку — «подписку выдаст
// администратор», владельцу — алерт.
func TestPerpetualInvite_PanelFailure(t *testing.T) {
	a, fm, fs, pp := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "p1", 1)
	pp.failW = true

	a.handleMessage(ctx, startMsg(pinvFriend, "Друг", "inv_p1"))

	if u, _ := fs.GetUser(ctx, pinvFriend); u == nil || !u.Whitelisted {
		t.Fatalf("Доступ не выдан при сбое панели: %+v", u)
	}
	all := fm.joined()
	if !strings.Contains(all, "подписку выдаст администратор") {
		t.Fatalf("человеку не сказано, что подписку выдаст администратор: %s", all)
	}
	if !strings.Contains(all, "Бессрочную подписку выдать не удалось") || !strings.Contains(all, strconv.FormatInt(pinvFriend, 10)) {
		t.Fatalf("владельцу нет алерта: %s", all)
	}
}

// N активаций работают, (N+1)-я — отказ.
func TestPerpetualInvite_NUses(t *testing.T) {
	a, fm, fs, pp := pinvApp(t)
	ctx := context.Background()
	perpetualInvite(t, fs, "pn", 2)

	a.handleMessage(ctx, startMsg(pinvFriend, "Один", "inv_pn"))
	a.handleMessage(ctx, startMsg(pinvFriend2, "Два", "inv_pn"))
	before := len(fm.texts)
	a.handleMessage(ctx, startMsg(pinvFriend3, "Три", "inv_pn"))

	for _, id := range []int64{pinvFriend, pinvFriend2} {
		if u, _ := fs.GetUser(ctx, id); u == nil || !u.Whitelisted {
			t.Fatalf("%d без Доступа", id)
		}
	}
	if u, _ := fs.GetUser(ctx, pinvFriend3); u != nil && u.Whitelisted {
		t.Fatal("третий получил Доступ по приглашению на 2 активации")
	}
	if posts, _ := pp.writes(); len(posts) != 2 {
		t.Fatalf("учёток создано %d, ожидалось 2", len(posts))
	}
	if !strings.Contains(strings.Join(fm.texts[before:], "\n"), "недействительна") {
		t.Fatalf("третьему нет отказа: %v", fm.texts[before:])
	}
}
