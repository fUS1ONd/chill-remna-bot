package app

import (
	"testing"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/model"
)

func TestIconizeKeyboard(t *testing.T) {
	icons := map[string]string{"💳": "111", "⚠️": "222", "⚠": "333"}
	rows := [][]models.InlineKeyboardButton{{
		{Text: "💳 Купить", CallbackData: "a"},
		{Text: "⚠️ Внимание", CallbackData: "b"},
		{Text: "Без эмодзи", CallbackData: "c"},
		{Text: "💳", CallbackData: "d"},
		{Text: "💳 Своя", CallbackData: "e", IconCustomEmojiID: "999"},
		{Text: "📦 Нет в карте", CallbackData: "f"},
	}}
	got := iconizeKeyboard(rows, icons)
	want := []struct{ text, icon string }{
		{"Купить", "111"},
		{"Внимание", "222"},
		{"Без эмодзи", ""},
		{"💳", ""},
		{"💳 Своя", "999"},
		{"📦 Нет в карте", ""},
	}
	for i, w := range want {
		b := got[0][i]
		if b.Text != w.text || b.IconCustomEmojiID != w.icon {
			t.Errorf("кнопка %d: %q/%q, ждали %q/%q", i, b.Text, b.IconCustomEmojiID, w.text, w.icon)
		}
	}
	if rows[0][0].Text != "💳 Купить" || rows[0][0].IconCustomEmojiID != "" {
		t.Error("исходная клавиатура изменилась")
	}
}

func TestIconizeKeyboardNoIcons(t *testing.T) {
	rows := [][]models.InlineKeyboardButton{{{Text: "💳 Купить"}}}
	if got := iconizeKeyboard(rows, nil); got[0][0].Text != "💳 Купить" {
		t.Errorf("без карты подпись изменилась: %q", got[0][0].Text)
	}
}

func TestMessengerKBIconsOff(t *testing.T) {
	m := botMessenger{icons: func() map[string]string { return nil }}
	got := m.kb([][]models.InlineKeyboardButton{{{Text: "💳 Купить"}}})
	if got[0][0].Text != "💳 Купить" || got[0][0].IconCustomEmojiID != "" {
		t.Errorf("выключенные значки поменяли кнопку: %+v", got[0][0])
	}
}

func TestMiniAppMenuRowHidden(t *testing.T) {
	a := &App{botCfg: &model.BotConfig{}}
	a.botCfg.MiniApp.Enabled = true
	a.botCfg.Webhook.PublicBaseURL = "https://app.example.com"
	if a.miniAppMenuRow("ru") == nil {
		t.Fatal("кнопка мини-аппа пропала без настройки")
	}
	a.botCfg.MiniApp.HideMenuButton = true
	if a.miniAppMenuRow("ru") != nil {
		t.Error("кнопка мини-аппа осталась в меню при hide_menu_button")
	}
	if a.miniAppButtonRow("ru") == nil {
		t.Error("hide_menu_button убрал кнопку и из напоминаний")
	}
}

func TestPackMenuRows(t *testing.T) {
	nav, extra := splitBalance([]models.InlineKeyboardButton{
		{CallbackData: "menu:mysubs"}, {CallbackData: "menu:renew"}, {CallbackData: "menu:balance"},
	})
	if btnData(nav) != "menu:mysubs|menu:renew|" || btnData(extra) != "menu:balance|" {
		t.Fatalf("splitBalance: %q / %q", btnData(nav), btnData(extra))
	}
	extra = append(extra, models.InlineKeyboardButton{CallbackData: "menu:uinv"},
		models.InlineKeyboardButton{CallbackData: "terms:docs"}, models.InlineKeyboardButton{URL: "https://t.me/x"},
		models.InlineKeyboardButton{URL: "https://t.me/y"})
	rows := packMenuRows(extra)
	if len(rows) != 3 || len(rows[0]) != 2 || len(rows[1]) != 2 || len(rows[2]) != 1 {
		t.Fatalf("раскладка: %+v", rows)
	}
	if packMenuRows(nil) != nil {
		t.Error("пустой список дал ряды")
	}
}
