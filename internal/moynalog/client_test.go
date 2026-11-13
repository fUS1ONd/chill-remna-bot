package moynalog

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
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
