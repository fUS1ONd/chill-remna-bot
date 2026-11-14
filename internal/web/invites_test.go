package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (f *fakeMini) MiniDenyText(context.Context, int64) string {
	return "Бот закрытый — войти можно только по приглашению от друга."
}

func miniToken(id int64) string {
	return issueJWT(id, false, jwtKey(fakeBotToken), time.Hour, 0, 0)
}

// Отказ в Доступе — машинно-различимый признак с понятным текстом: по нему
// фронт рисует экран отказа, а не «Не удалось загрузить».
func TestAccessDeniedMarker(t *testing.T) {
	f := &fakeMini{denied: true}
	h := gateServer(t, f)
	for i, tok := range []string{miniToken(555), webToken(f, 555, 0)} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, gateReq(http.MethodGet, "/api/miniapp/me", "", tok, "203.0.113."+strconv.Itoa(60+i)))
		var res struct {
			Error        string `json:"error"`
			AccessDenied bool   `json:"access_denied"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || w.Code != http.StatusForbidden || !res.AccessDenied {
			t.Fatalf("пропуск %d: code=%d тело %s", i, w.Code, w.Body.String())
		}
		if res.Error != "Бот закрытый — войти можно только по приглашению от друга." {
			t.Fatalf("текст отказа: %q", res.Error)
		}
	}
	// Прочие отказы признака не несут.
	f.denied = false
	f.blocked = true
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gateReq(http.MethodPost, "/api/miniapp/checkout", `{"months":1}`, webToken(f, -7, 0), "203.0.113.62"))
	if w.Code != http.StatusForbidden || json.Valid(w.Body.Bytes()) && containsKey(w.Body.Bytes(), "access_denied") {
		t.Fatalf("гейт почты с признаком отказа в Доступе: %d %s", w.Code, w.Body.String())
	}
}

func containsKey(b []byte, key string) bool {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	_, ok := m[key]
	return ok
}

// MiniStart запоминает параметр входа; Приглашение «выдаёт» Доступ.
func (f *fakeMini) MiniStart(_ context.Context, _ int64, param, username, firstName string, _ bool) string {
	f.tgStart = param
	f.tgName = username + "|" + firstName
	if strings.HasPrefix(param, "inv_") {
		f.denied = false
	}
	return ""
}
func (f *fakeMini) CabinetEnsureUser(context.Context, int64) {}

// Вход в кабинет через Telegram без Доступа — тот же признак отказа.
func TestCabinetTelegramLoginDenied(t *testing.T) {
	f := &fakeMini{denied: true}
	h := gateServer(t, f)
	now := time.Now().Unix()
	fields := map[string]string{"id": "900", "auth_date": strconv.FormatInt(now, 10)}
	body, _ := json.Marshal(map[string]any{"id": 900, "auth_date": now, "hash": signLogin(fields, fakeBotToken)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gateReq(http.MethodPost, "/api/cabinet/auth/telegram", string(body), "", "203.0.113.70"))
	if w.Code != http.StatusForbidden || !containsKey(w.Body.Bytes(), "access_denied") || containsKey(w.Body.Bytes(), "token") {
		t.Fatalf("вход без Доступа: %d %s", w.Code, w.Body.String())
	}
}

func (f *fakeMini) MiniInvites(_ context.Context, id int64) MiniInvitesDTO {
	f.invites = append(f.invites, "list:"+strconv.FormatInt(id, 10))
	return MiniInvitesDTO{Allowed: true, MaxActive: 3, Invites: []MiniInviteDTO{{Code: "c1", Link: "https://t.me/testbot?start=inv_c1"}}}
}
func (f *fakeMini) MiniInviteCreate(_ context.Context, id int64) MiniInvitesDTO {
	f.invites = append(f.invites, "new:"+strconv.FormatInt(id, 10))
	return MiniInvitesDTO{Allowed: true, OK: true}
}
func (f *fakeMini) MiniInviteRevoke(_ context.Context, id int64, code string) MiniInvitesDTO {
	f.invites = append(f.invites, "revoke:"+strconv.FormatInt(id, 10)+":"+code)
	return MiniInvitesDTO{Allowed: true, OK: true}
}

// Ручки Приглашений: список, создание и отзыв доходят до приложения от имени
// владельца пропуска; гейт почты на них не стоит (денежно нейтральные), а
// Закрытый вход — стоит.
func TestInviteEndpoints(t *testing.T) {
	f := &fakeMini{blocked: true}
	h := gateServer(t, f)
	tok := webToken(f, 555, 0)
	steps := []struct{ method, path, body string }{
		{http.MethodGet, "/api/miniapp/invites", ""},
		{http.MethodPost, "/api/miniapp/invites/new", "{}"},
		{http.MethodPost, "/api/miniapp/invites/revoke", `{"code":"c1"}`},
	}
	for i, st := range steps {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, gateReq(st.method, st.path, st.body, tok, "203.0.113."+strconv.Itoa(80+i)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", st.path, w.Code, w.Body.String())
		}
	}
	want := []string{"list:555", "new:555", "revoke:555:c1"}
	if len(f.invites) != 3 || f.invites[0] != want[0] || f.invites[1] != want[1] || f.invites[2] != want[2] {
		t.Fatalf("вызовы: %v", f.invites)
	}
	// Отзыв без кода — плохой запрос, до приложения не доходит.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gateReq(http.MethodPost, "/api/miniapp/invites/revoke", `{}`, tok, "203.0.113.84"))
	if w.Code != http.StatusBadRequest || len(f.invites) != 3 {
		t.Fatalf("отзыв без кода: %d %v", w.Code, f.invites)
	}
	f.denied = true
	for i, st := range steps {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, gateReq(st.method, st.path, st.body, tok, "203.0.113."+strconv.Itoa(90+i)))
		if w.Code != http.StatusForbidden || !containsKey(w.Body.Bytes(), "access_denied") {
			t.Fatalf("%s без Доступа: %d %s", st.path, w.Code, w.Body.String())
		}
	}
	if len(f.invites) != 3 {
		t.Fatalf("без Доступа дошло до приложения: %v", f.invites)
	}
}

// Вход в кабинет через Telegram по ссылке с ?inv=: Приглашение гасится до
// проверки Доступа, и человек входит, а не получает отказ.
func TestCabinetTelegramLoginInvite(t *testing.T) {
	f := &fakeMini{denied: true}
	h := gateServer(t, f)
	now := time.Now().Unix()
	fields := map[string]string{"id": "900", "auth_date": strconv.FormatInt(now, 10), "username": "friend_u", "first_name": "Друг"}
	body, _ := json.Marshal(map[string]any{"id": 900, "auth_date": now, "username": "friend_u", "first_name": "Друг",
		"hash": signLogin(fields, fakeBotToken), "start": "inv_c1"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gateReq(http.MethodPost, "/api/cabinet/auth/telegram", string(body), "", "203.0.113.71"))
	if w.Code != http.StatusOK || !containsKey(w.Body.Bytes(), "token") || f.tgStart != "inv_c1" {
		t.Fatalf("вход с приглашением: %d %s start=%q", w.Code, w.Body.String(), f.tgStart)
	}
	// Имя из подписи виджета доходит до Приглашения (QA B1).
	if f.tgName != "friend_u|Друг" {
		t.Fatalf("имя до MiniStart: %q", f.tgName)
	}
}

// Старт мини-аппа с inv_: имя из initData доходит до Приглашения (QA B1).
func TestMiniAuthInviteName(t *testing.T) {
	f := &fakeMini{}
	h := gateServer(t, f)
	v := url.Values{}
	v.Set("auth_date", strconv.FormatInt(time.Now().Unix(), 10))
	v.Set("user", `{"id":901,"first_name":"Друг","username":"friend_u"}`)
	v.Set("start_param", "inv_c1")
	body, _ := json.Marshal(map[string]string{"init_data": signInitData(v, fakeBotToken)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, gateReq(http.MethodPost, "/api/miniapp/auth", string(body), "", "203.0.113.72"))
	if w.Code != http.StatusOK || f.tgStart != "inv_c1" || f.tgName != "friend_u|Друг" {
		t.Fatalf("вход мини-аппа: %d %s start=%q name=%q", w.Code, w.Body.String(), f.tgStart, f.tgName)
	}
}
