package app

import (
	"context"
	"time"

	"remnabot/internal/i18n"
	"remnabot/internal/web"
)

// perpetualFrom — граница Бессрочной подписки: срок окончания не раньше этой
// даты. Флага в базе нет (docs/adr/0003): перенесённые из старого бота
// «вечные» учётки (срок 2099) и выданные Безлимит-приглашением узнаются
// одинаково, а владелец делает человека бессрочным, просто сдвинув срок в
// панели.
var perpetualFrom = time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)

// perpetualExpiry — бессрочен ли срок окончания из панели (RFC3339).
// Пустой и нечитаемый срок бессрочным не считается.
func perpetualExpiry(expireAt string) bool {
	t, err := time.Parse(time.RFC3339, expireAt)
	return err == nil && !t.Before(perpetualFrom)
}

// subPerpetual — единственное место, где решается, Бессрочная ли подписка у
// человека; им пользуются бот, мини-апп, кабинет, рефералка и напоминания.
// Истина — срок в панели (через тот же кэш, что и «есть ли подписка»); пока
// панель не ответила ни разу — срок из базы бота.
func (a *App) subPerpetual(ctx context.Context, tgID int64) bool {
	if has, exp, known := a.subCached(ctx, tgID); known {
		return has && perpetualExpiry(exp)
	}
	if a.store == nil {
		return false
	}
	u, _ := a.store.GetUser(ctx, tgID)
	return u != nil && perpetualExpiry(u.SubExpireAt)
}

// perpetualHome закрывает бессрочному экраны покупки, продления, триала,
// автопродления и кошелька: их кнопки живут и в старых сообщениях, а витрина
// открывается ещё и /buy и ссылкой на тариф. Бессрочного возвращает в меню и
// отвечает true.
func (a *App) perpetualHome(ctx context.Context, chatID int64) bool {
	if !a.subPerpetual(ctx, chatID) {
		return false
	}
	a.showMenu(ctx, chatID, false, a.displayNameByID(ctx, chatID))
	return true
}

// formatSubExpire — срок подписки для показа: у Бессрочной вместо даты
// «Бессрочная».
func formatSubExpire(raw, lang string) string {
	if perpetualExpiry(raw) {
		return i18n.T(lang, "sub.perpetual")
	}
	return formatExpire(raw, lang)
}

// miniPerpetualRefused — отказ мини-аппа и кабинета на покупку, продление,
// триал, автопродление и пополнение у бессрочного: кнопок фронт не рисует,
// это ответ на прямой запрос.
func (a *App) miniPerpetualRefused(ctx context.Context, tgID int64) (web.MiniActionDTO, bool) {
	if !a.subPerpetual(ctx, tgID) {
		return web.MiniActionDTO{}, false
	}
	return web.MiniActionDTO{Error: stripHTMLTags(i18n.T(a.lang(tgID), "mini.perpetual"))}, true
}
