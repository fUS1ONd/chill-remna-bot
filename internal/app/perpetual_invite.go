package app

import (
	"context"
	"errors"
	"html"
	"strconv"
	"time"

	"remnabot/internal/i18n"
	"remnabot/internal/remnawave"
)

// perpetualUntil — срок, который выдаёт Безлимит-приглашение. Бессрочной
// подписку делает граница perpetualFrom, этот срок лежит за ней с запасом.
var perpetualUntil = time.Date(2099, 12, 31, 0, 0, 0, 0, time.UTC)

// perpetualLimitsLocked — лимиты новой учётки по Безлимит-приглашению:
// сквады, устройства и стратегия тарифа «Базовый» (уровень тарифа, без
// переопределений сроков). Вызывать под a.mu.
func (a *App) perpetualLimitsLocked() remnawave.UserLimits {
	p := basePlanFrom(a.botCfg, nil)
	if p == nil {
		return remnawave.UserLimits{}
	}
	return remnawave.UserLimits{
		InternalSquads: p.IntSquads,
		ExternalSquad:  p.ExtSquad,
		DeviceLimit:    p.DeviceLimit,
		Strategy:       p.Strategy,
	}
}

// grantPerpetual выдаёт Бессрочную подписку в панели и записывает срок в базу
// бота; запись автопродления удаляется целиком — платить больше не за что.
// Замки: автосписание человека (autoPayLk) снаружи, выдача подписки
// (finalizeUserLk) внутри — тот же порядок, что у автосписания.
func (a *App) grantPerpetual(ctx context.Context, chatID int64) error {
	a.mu.Lock()
	panel := a.panel
	limits := a.perpetualLimitsLocked()
	a.mu.Unlock()
	if panel == nil {
		return errors.New("панель не подключена")
	}
	apLk := a.autoPayLock(chatID)
	apLk.Lock()
	defer apLk.Unlock()
	ulk := &a.finalizeUserLk[extLockIndex(strconv.FormatInt(chatID, 10))]
	ulk.Lock()
	_, expireAt, err := panel.GrantPerpetual(ctx, chatID, perpetualUntil.Format(time.RFC3339), limits)
	ulk.Unlock()
	if err != nil {
		return err
	}
	a.invalidateSubCache(chatID)
	if a.store == nil {
		return nil
	}
	if expireAt == "" {
		expireAt = perpetualUntil.Format(time.RFC3339)
	}
	if err := a.store.SetSubExpiry(ctx, chatID, expireAt, "paid"); err != nil {
		// Истина — панель: срок в базе бота догонит синхронизация.
		a.log.Warn("безлимит: срок не записан в базу", "tg_id", chatID, "err", err)
	}
	if ap, _ := a.store.GetAutoPay(ctx, chatID); ap != nil {
		if err := a.store.DeleteAutoPay(ctx, chatID); err != nil {
			a.log.Warn("безлимит: запись автопродления не удалена", "tg_id", chatID, "err", err)
		} else {
			a.payLog(ctx, ap.Method, "", chatID, "autopay_unlinked", "Бессрочная по Безлимит-приглашению")
		}
	}
	return nil
}

// grantPerpetualByInvite — выдача по Безлимит-приглашению после того, как
// активация списана и Доступ есть. Возвращает текст для человека; при сбое
// панели владелец получает алерт и выдаёт подписку руками.
func (a *App) grantPerpetualByInvite(ctx context.Context, chatID int64, code string) string {
	lang := a.lang(chatID)
	alang := a.lang(a.cfg.AdminID)
	if err := a.grantPerpetual(ctx, chatID); err != nil {
		a.log.Error("безлимит: подписка не выдана", "tg_id", chatID, "code", code, "err", err)
		// Фоновый контекст: запрос мини-аппа мог уже отмениться, а алерт —
		// единственный сигнал владельцу.
		bg := a.bgContext()
		a.notify(bg, a.cfg.AdminID, i18n.T(alang, "inv.perpetual_failed_admin", code, a.userLabelByID(bg, chatID), cutHTML(html.EscapeString(err.Error()), 200)))
		return i18n.T(lang, "inv.perpetual_pending")
	}
	a.log.Info("безлимит: Бессрочная выдана", "tg_id", chatID, "code", code)
	a.notify(ctx, a.cfg.AdminID, i18n.T(alang, "inv.used_admin", code, a.userLabelByID(ctx, chatID)))
	return i18n.T(lang, "inv.perpetual_ok")
}
