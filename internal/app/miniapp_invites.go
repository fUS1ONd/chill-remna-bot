package app

import (
	"context"
	"time"

	"remnabot/internal/i18n"
	"remnabot/internal/web"
)

// Экран «Пригласить друга» мини-аппа и кабинета — переходники к логике
// чата (user_invites.go): те же право, лимиты, тексты и отзыв.

// MiniInvites — экран приглашений: бонусы, квоты, активные Приглашения.
func (a *App) MiniInvites(ctx context.Context, tgID int64) web.MiniInvitesDTO {
	lang := a.lang(tgID)
	if !a.canInviteFriends(ctx, tgID) {
		return web.MiniInvitesDTO{Error: stripHTMLTags(i18n.T(lang, "uinv.not_allowed"))}
	}
	list, err := a.store.ListInvitesBy(ctx, tgID)
	if err != nil {
		a.log.Warn("user invite: список", "tg_id", tgID, "err", err)
		return web.MiniInvitesDTO{Error: stripHTMLTags(i18n.T(lang, "uinv.fail"))}
	}
	now := time.Now().UTC()
	d := web.MiniInvitesDTO{
		Allowed:   true,
		MaxActive: userInviteMaxActive,
		TTLDays:   int(userInviteTTL / (24 * time.Hour)),
		Invites:   []web.MiniInviteDTO{},
	}
	d.Active, _ = userInviteQuota(list, now)
	if cfg, count, earned, ok := a.userInviteBonusData(ctx, tgID); ok {
		d.Bonus = &web.MiniInviteBonusDTO{
			BonusValue:    cfg.BonusValue,
			BonusKind:     cfg.BonusKind,
			OnFirstPay:    cfg.OnFirstPay,
			InviteeKind:   cfg.InviteeKind,
			InviteeValue:  cfg.InviteeValue,
			Percent:       cfg.Percent,
			Count:         count,
			EarnedKopecks: earned,
		}
	}
	for i := range list {
		inv := &list[i]
		if !inv.Active(now) {
			continue
		}
		link := a.inviteLink(ctx, inv.Code)
		d.Invites = append(d.Invites, web.MiniInviteDTO{
			Code:        inv.Code,
			Link:        link,
			CabinetLink: a.inviteCabinetLink(inv.Code),
			ShareURL:    a.inviteShareURL(ctx, lang, inv.Code),
			ShareText:   a.inviteShareMessage(lang, inv.Code, link),
			ExpireAt:    formatExpire(inv.ExpiresAt, lang),
		})
	}
	return d
}

// MiniInviteCreate — «➕ Создать приглашение»: экран после создания.
func (a *App) MiniInviteCreate(ctx context.Context, tgID int64) web.MiniInvitesDTO {
	lang := a.lang(tgID)
	_, reason := a.createUserInvite(ctx, tgID)
	d := a.MiniInvites(ctx, tgID)
	if reason != "" {
		d.Error = stripHTMLTags(userInviteRefusal(lang, reason))
		return d
	}
	d.OK, d.Message = true, stripHTMLTags(i18n.T(lang, "uinv.created"))
	return d
}

// MiniInviteRevoke — «🚫 Отозвать»: экран после отзыва.
func (a *App) MiniInviteRevoke(ctx context.Context, tgID int64, code string) web.MiniInvitesDTO {
	lang := a.lang(tgID)
	ok := a.revokeUserInvite(ctx, tgID, code)
	d := a.MiniInvites(ctx, tgID)
	if !ok {
		d.Error = stripHTMLTags(i18n.T(lang, "uinv.revoke_none"))
		return d
	}
	d.OK, d.Message = true, stripHTMLTags(i18n.T(lang, "uinv.revoked"))
	return d
}
