package moynalog

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fnsStub — кабинет ФНС: логин выдаёт токен, /income отвечает тем, что задал тест.
func fnsStub(t *testing.T, income http.HandlerFunc) (incomeCalls *atomic.Int32) {
	t.Helper()
	incomeCalls = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/lkfl":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok123"})
		case "/income":
			incomeCalls.Add(1)
			income(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	prev := BaseURL
	BaseURL = srv.URL
	t.Cleanup(func() { BaseURL = prev })
	return incomeCalls
}

func TestCreateIncome(t *testing.T) {
	var body incomeRequest
	calls := fnsStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]string{"approvedReceiptUuid": "rcpt-42"})
	})

	at := time.Date(2026, 11, 30, 21, 30, 0, 0, time.UTC)
	id, err := New("79990000000", "secret").CreateIncome(context.Background(),
		Income{Amount: 150, Name: "VPN 1 мес (abc123)", OperationTime: at})
	if err != nil || id != "rcpt-42" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("income вызван %d раз", calls.Load())
	}
	// Чек датируется моментом дохода по Москве: 21:30 UTC 30-го — уже 1-е число.
	if body.OperationTime != "2026-12-01T00:30:00+03:00" {
		t.Fatalf("operationTime = %q", body.OperationTime)
	}
	if len(body.Services) != 1 || body.Services[0].Name != "VPN 1 мес (abc123)" || body.TotalAmount != "150.00" {
		t.Fatalf("тело запроса: %+v", body)
	}
}

// Исход обращения определяет, что делать с чеком дальше: пробит, судьба
// неизвестна (сверка по метке) или отказ навсегда. Повтор /income внутри
// одного вызова недопустим: после потерянного ответа он и есть дубль чека.
func TestCreateIncomeOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		income http.HandlerFunc
		want   Kind
	}{
		{"нечитаемый ответ", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>"))
		}, KindUnknown},
		{"ответ без uuid", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}, KindUnknown},
		{"ответ оборван", func(w http.ResponseWriter, _ *http.Request) {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}, KindUnknown},
		{"отказ 4xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"validation.failed"}`))
		}, KindRejected},
		{"сбой 5xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, KindRetry},
		{"слишком часто", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}, KindRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := fnsStub(t, tc.income)
			_, err := New("l", "p").CreateIncome(context.Background(), Income{Amount: 10, Name: "x"})
			if err == nil {
				t.Fatal("ожидалась ошибка")
			}
			if got := KindOf(err); got != tc.want {
				t.Fatalf("класс ошибки %v, ожидался %v (err=%v)", got, tc.want, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("/income вызван %d раз, повторов внутри вызова быть не должно", calls.Load())
			}
		})
	}
}

// Не удалось даже соединиться — запрос не ушёл, чека точно нет: это не
// «неизвестно», а обычный повтор.
func TestCreateIncomeDialFailureIsRetry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	prev := BaseURL
	BaseURL = "http://" + addr
	defer func() { BaseURL = prev }()

	c := New("l", "p")
	c.token = "tok"
	_, err = c.CreateIncome(context.Background(), Income{Amount: 10, Name: "x"})
	if KindOf(err) != KindRetry {
		t.Fatalf("отказ в соединении: класс %v, err=%v", KindOf(err), err)
	}
}

// Протухший токен: ФНС отвергла запрос, чек не создан — перелогин и ровно
// одна повторная отправка.
func TestCreateIncomeRelogin(t *testing.T) {
	var n atomic.Int32
	calls := fnsStub(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"approvedReceiptUuid": "rcpt-1"})
	})
	c := New("l", "p")
	c.token = "old"
	id, err := c.CreateIncome(context.Background(), Income{Amount: 10, Name: "x"})
	if err != nil || id != "rcpt-1" || calls.Load() != 2 {
		t.Fatalf("id=%q err=%v calls=%d", id, err, calls.Load())
	}
	if !strings.Contains(c.token, "tok123") {
		t.Fatalf("токен не обновился: %q", c.token)
	}
}

// incomesStub — кабинет ФНС, отдающий на /incomes тело body; в q — запрос.
func incomesStub(t *testing.T, body string, q *url.Values) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/lkfl":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok123"})
		case "/incomes":
			if r.Header.Get("Authorization") != "Bearer tok123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			*q = r.URL.Query()
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	prev := BaseURL
	BaseURL = srv.URL
	t.Cleanup(func() { BaseURL = prev })
}

// Список доходов разбирается, метка ищется в наименовании чека и его услуг,
// границы уходят по Москве.
func TestListIncomes(t *testing.T) {
	var q url.Values
	incomesStub(t, `{"content":[`+
		`{"approvedReceiptUuid":"r1","name":"VPN (abc123)","services":[{"name":"VPN (abc123)"}]},`+
		`{"approvedReceiptUuid":"r2","name":"","services":[{"name":"VPN (zzz999)"}],"cancellationInfo":{"comment":"возврат"}}`+
		`],"hasMore":false}`, &q)

	from := time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)
	list, complete, err := New("l", "p").ListIncomes(context.Background(), from, from.Add(48*time.Hour))
	if err != nil || !complete || len(list) != 2 {
		t.Fatalf("list=%+v complete=%v err=%v", list, complete, err)
	}
	if q.Get("from") != "2026-11-01T00:00:00+03:00" || q.Get("to") != "2026-11-03T00:00:00+03:00" {
		t.Fatalf("границы выборки: %v", q)
	}
	if !list[0].Matches("abc123") || list[0].Matches("zzz999") || list[0].UUID != "r1" || list[0].Canceled {
		t.Fatalf("первый доход: %+v", list[0])
	}
	if !list[1].Matches("zzz999") || !list[1].Canceled {
		t.Fatalf("метка в услуге и аннулирование: %+v", list[1])
	}
	if list[0].Matches("") || list[0].Matches("abc") {
		t.Fatal("пустая или частичная метка не должна совпадать")
	}
}

// Список обходится по страницам; полным он считается, только если дошли до
// конца. Упёрлись в потолок страниц — неполный.
func TestListIncomesPages(t *testing.T) {
	page := func(n int, more bool) string {
		items := make([]string, n)
		for i := range items {
			items[i] = `{"approvedReceiptUuid":"r","name":"x"}`
		}
		m := "false"
		if more {
			m = "true"
		}
		return `{"content":[` + strings.Join(items, ",") + `],"hasMore":` + m + `}`
	}
	for name, tc := range map[string]struct {
		pages    func(offset int) string
		n        int
		complete bool
	}{
		"hasMore, потом конец": {func(o int) string {
			if o == 0 {
				return page(1, true)
			}
			return page(1, false)
		}, 2, true},
		"полная страница без hasMore": {func(o int) string {
			if o == 0 {
				return page(incomesLimit, false)
			}
			return page(0, false)
		}, incomesLimit, true},
		"потолок страниц": {func(int) string { return page(1, true) }, incomesMaxPages, false},
	} {
		t.Run(name, func(t *testing.T) {
			var offsets []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth/lkfl":
					_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok123"})
				case "/incomes":
					offsets = append(offsets, r.URL.Query().Get("offset"))
					o, _ := strconv.Atoi(r.URL.Query().Get("offset"))
					_, _ = w.Write([]byte(tc.pages(o)))
				}
			}))
			t.Cleanup(srv.Close)
			prev := BaseURL
			BaseURL = srv.URL
			t.Cleanup(func() { BaseURL = prev })

			list, complete, err := New("l", "p").ListIncomes(context.Background(), time.Now(), time.Now())
			if err != nil || complete != tc.complete || len(list) != tc.n {
				t.Fatalf("len=%d complete=%v err=%v offsets=%v", len(list), complete, err, offsets)
			}
		})
	}
}
