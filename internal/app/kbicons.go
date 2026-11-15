package app

import (
	"sort"
	"strings"

	"github.com/go-telegram/bot/models"
)

// iconizeKeyboard переносит эмодзи из начала подписи кнопки в значок
// премиум-эмодзи (icon_custom_emoji_id), если для этого эмодзи в карте есть
// id. Подпись без эмодзи в начале, кнопка с уже заданным значком и пустая
// после снятия эмодзи подпись не меняются. Исходные срезы не трогаются:
// клавиатуры экранов бывают общими.
func iconizeKeyboard(rows [][]models.InlineKeyboardButton, icons map[string]string) [][]models.InlineKeyboardButton {
	if len(icons) == 0 || len(rows) == 0 {
		return rows
	}
	keys := make([]string, 0, len(icons))
	for e, id := range icons {
		if e != "" && id != "" {
			keys = append(keys, e)
		}
	}
	if len(keys) == 0 {
		return rows
	}
	// Длинные первыми: «⚠️» не должен совпасть как «⚠» с хвостом U+FE0F.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	out := make([][]models.InlineKeyboardButton, len(rows))
	for i, row := range rows {
		nr := make([]models.InlineKeyboardButton, len(row))
		for j, b := range row {
			nr[j] = iconizeButton(b, keys, icons)
		}
		out[i] = nr
	}
	return out
}

func iconizeButton(b models.InlineKeyboardButton, keys []string, icons map[string]string) models.InlineKeyboardButton {
	if b.IconCustomEmojiID != "" {
		return b
	}
	text := strings.TrimLeft(b.Text, " ")
	for _, k := range keys {
		if !strings.HasPrefix(text, k) {
			continue
		}
		rest := strings.TrimPrefix(text[len(k):], "️")
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return b
		}
		b.Text = rest
		b.IconCustomEmojiID = icons[k]
		return b
	}
	return b
}
