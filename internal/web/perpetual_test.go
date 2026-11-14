package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// Фронт обоих дизайнов прячет кошелёк и дату по полю perpetual в ответах
// /api/miniapp/menu и /api/miniapp/subscription; у обычного платника поля нет.
func TestPerpetualJSON(t *testing.T) {
	for name, v := range map[string]any{
		"menu": MiniMenuDTO{HasSub: true, Perpetual: true},
		"sub":  MiniSubDTO{Active: true, Perpetual: true, ExpireAt: "Бессрочная"},
	} {
		b, _ := json.Marshal(v)
		if !strings.Contains(string(b), `"perpetual":true`) {
			t.Errorf("%s: нет perpetual: %s", name, b)
		}
	}
	b, _ := json.Marshal(MiniMenuDTO{HasSub: true})
	if strings.Contains(string(b), "perpetual") {
		t.Errorf("у обычного платника есть perpetual: %s", b)
	}
}
