package remnawave

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"remnabot/internal/model"
)

func subpageClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(model.PanelConfig{Mode: model.ModeRemote, BaseURL: srv.URL, APIToken: "t"})
}

func TestSubpageConfigsOrdersByViewPosition(t *testing.T) {
	c := subpageClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/subscription-page-configs" {
			t.Errorf("путь = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"response":{"total":2,"configs":[
			{"uuid":"b","name":"Second","viewPosition":2,"config":{"platforms":{}}},
			{"uuid":"a","name":"Default","viewPosition":1,"config":{"platforms":{}}}
		]}}`)
	})
	list, err := c.SubpageConfigs(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(list) != 2 || list[0].UUID != "a" || list[1].UUID != "b" {
		t.Fatalf("порядок = %+v", list)
	}
	if len(list[0].Config) == 0 {
		t.Error("сырой конфиг не сохранён")
	}
}

// Панель 3.4 в списке отдаёт config: null — сам конфиг приходит только
// запросом по uuid. Без дозапроса мини-апп не видит ни одного приложения.
func TestSubpageConfigsFetchesNullConfigByUUID(t *testing.T) {
	c := subpageClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/subscription-page-configs":
			_, _ = io.WriteString(w, `{"response":{"total":2,"configs":[
				{"uuid":"a","name":"Default","viewPosition":1,"config":null},
				{"uuid":"b","name":"Inline","viewPosition":2,"config":{"platforms":{"x":1}}}
			]}}`)
		case "/api/subscription-page-configs/a":
			_, _ = io.WriteString(w, `{"response":{"uuid":"a","name":"Default","viewPosition":1,"config":{"platforms":{"ios":1}}}}`)
		default:
			t.Errorf("лишний запрос %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	list, err := c.SubpageConfigs(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(list) != 2 || !strings.Contains(string(list[0].Config), `"ios"`) {
		t.Fatalf("конфиг по uuid не подтянут: %+v", list)
	}
	if !strings.Contains(string(list[1].Config), `"x"`) {
		t.Fatalf("конфиг из списка потерян: %s", list[1].Config)
	}
}

// Дозапрос не удался — ошибка наверх: вызывающий поставит паузу и спросит
// позже, а не закэширует «приложений нет».
func TestSubpageConfigsByUUIDError(t *testing.T) {
	c := subpageClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/subscription-page-configs" {
			_, _ = io.WriteString(w, `{"response":{"total":1,"configs":[{"uuid":"a","name":"D","config":null}]}}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.SubpageConfigs(context.Background()); err == nil {
		t.Fatal("ошибка дозапроса проглочена")
	}
}

// Панель до 3.0.0 такого маршрута не знает — это не ошибка, а сигнал идти
// старым путём, на саму страницу подписки.
func TestSubpageConfigsMissingAPI(t *testing.T) {
	c := subpageClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	_, err := c.SubpageConfigs(context.Background())
	if !ErrNoSubpageAPI(err) {
		t.Fatalf("err = %v, want «нет API»", err)
	}
}

func TestSubpageConfigUUIDForReadsAssignment(t *testing.T) {
	c := subpageClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/subpage-config/"); got != "sh0rt" {
			t.Errorf("shortUuid = %q", got)
		}
		_, _ = io.WriteString(w, `{"response":{"subpageConfigUuid":"b","webpageAllowed":true}}`)
	})
	uuid, err := c.SubpageConfigUUIDFor(context.Background(), "sh0rt")
	if err != nil || uuid != "b" {
		t.Fatalf("uuid = %q, err = %v", uuid, err)
	}
}

// null означает «конфиг не назначен» — берём дефолтный, а не падаем.
func TestSubpageConfigUUIDForNull(t *testing.T) {
	c := subpageClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"response":{"subpageConfigUuid":null,"webpageAllowed":true}}`)
	})
	uuid, err := c.SubpageConfigUUIDFor(context.Background(), "sh0rt")
	if err != nil || uuid != "" {
		t.Fatalf("uuid = %q, err = %v", uuid, err)
	}
}

func TestShortUUIDParsedFromPanelUser(t *testing.T) {
	u := toPanelUser(&panelUser{Uuid: "u-1", ShortUuid: "sh0rt", Username: "tg_1"})
	if u == nil || u.ShortUUID != "sh0rt" {
		t.Fatalf("ShortUUID = %+v", u)
	}
}
