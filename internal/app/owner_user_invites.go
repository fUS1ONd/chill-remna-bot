package app

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/assets"
	"remnabot/internal/i18n"
	"remnabot/internal/model"
)

// Вид владельца на Приглашения пользователей: отдельный список в разделе
// доступа и блок в карточке пользователя. Свой список владельца (acc:list)
// показывает только его приглашения.

// showOwnerUserInvites — активные Приглашения пользователей с Автором и счётчики
// «создано / активировано / из них оплатили».
func (a *App) showOwnerUserInvites(ctx context.Context, chatID int64, page int) {
	lang := a.lang(chatID)
	if a.store == nil {
		return
	}
	all, err := a.store.ListInvites(ctx)
	if err != nil {
		a.sendHome(ctx, chatID, "❌ "+err.Error())
		return
	}
	stats, err := a.store.UserInviteStats(ctx)
	if err != nil {
		a.sendHome(ctx, chatID, "❌ "+err.Error())
		return
	}
	now := time.Now().UTC()
	var list []model.Invite
	for _, inv := range all {
		if inv.FromUser() && inv.Active(now) {
			list = append(list, inv)
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt > list[j].CreatedAt
		}
		return list[i].Code < list[j].Code
	})
	pages := (len(list) + invitesPageSize - 1) / invitesPageSize
	if page >= pages {
		page = pages - 1
	}
	if page < 0 {
		page = 0
	}
	from := min(page*invitesPageSize, len(list))
	to := min(from+invitesPageSize, len(list))
	var rows [][]models.InlineKeyboardButton
	for _, inv := range list[from:to] {
		label := i18n.T(lang, "inv.state_active") + " " + inv.Code + " · " + a.userButtonLabelByID(ctx, inv.CreatedBy)
		if inv.ExpiresAt != "" {
			label += " · " + shortDate(inv.ExpiresAt)
		}
		rows = append(rows, []models.InlineKeyboardButton{btn(label, "acc:show:"+inv.Code)})
	}
	if nav := paginationRow("acc:upage:", page, pages, i18n.T(lang, "btn.prev"), i18n.T(lang, "btn.next")); len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []models.InlineKeyboardButton{
		btn(i18n.T(lang, "btn.back"), "menu:access"), btn(i18n.T(lang, "btn.home"), "menu:home"),
	})
	text := i18n.T(lang, "uinv.owner_title", len(list), stats.Created, stats.Activated, stats.Paid)
	a.sendKBSection(ctx, chatID, assets.SectionReferral, text, rows)
}

// userButtonLabelByID — подпись человека для кнопки; нет карточки — его ID.
func (a *App) userButtonLabelByID(ctx context.Context, id int64) string {
	if u, _ := a.store.GetUser(ctx, id); u != nil {
		return a.userButtonLabel(ctx, u)
	}
	return strconv.FormatInt(id, 10)
}

// userInviteBlock — строки и кнопки карточки пользователя про приглашения:
// Пригласивший, приглашённые им и его активные Приглашения с отзывом.
func (a *App) userInviteBlock(ctx context.Context, lang string, u *model.User) (string, [][]models.InlineKeyboardButton) {
	id := strconv.FormatInt(u.TelegramID, 10)
	var text strings.Builder
	var rows [][]models.InlineKeyboardButton
	if u.ReferredBy != 0 {
		text.WriteString("\n" + i18n.T(lang, "user.inviter", a.userLabelByID(ctx, u.ReferredBy)))
		rows = append(rows, []models.InlineKeyboardButton{
			btn(i18n.T(lang, "user.btn_inviter"), "usr:view:"+strconv.FormatInt(u.ReferredBy, 10)),
		})
	}
	if n, err := a.store.CountReferrals(ctx, u.TelegramID); err == nil && n > 0 {
		rows = append(rows, []models.InlineKeyboardButton{
			btn(i18n.T(lang, "user.btn_referrals", n), "usr:refs:"+id+":0"),
		})
	}
	if list, err := a.store.ListInvitesBy(ctx, u.TelegramID); err == nil {
		now := time.Now().UTC()
		active := 0
		for _, inv := range list {
			if !inv.Active(now) {
				continue
			}
			active++
			exp := i18n.T(lang, "inv.no_expiry")
			if inv.ExpiresAt != "" {
				exp = shortDate(inv.ExpiresAt)
			}
			rows = append(rows, []models.InlineKeyboardButton{
				btn(i18n.T(lang, "user.btn_uinv_revoke", inv.Code, exp), "usr:urev:"+id+":"+inv.Code),
			})
		}
		if active > 0 {
			text.WriteString("\n" + i18n.T(lang, "user.uinv_active", active))
		}
	}
	return text.String(), rows
}

// revokeUserInviteOf — отзыв владельцем из карточки человека: только
// Приглашение этого человека.
func (a *App) revokeUserInviteOf(ctx context.Context, uid int64, code string) {
	if uid == 0 || code == "" {
		return
	}
	inv, err := a.store.GetInvite(ctx, code)
	if err != nil || inv == nil || inv.CreatedBy != uid {
		return
	}
	if err := a.store.RevokeInvite(ctx, code); err != nil {
		a.log.Warn("uinv: отзыв владельцем из карточки", "code", code, "err", err)
	}
}

// referralsPageSize — сколько приглашённых показываем на одной странице.
const referralsPageSize = 10

// showReferrals — приглашённые человеком: кнопки ведут в их карточки.
func (a *App) showReferrals(ctx context.Context, chatID, uid int64, page int) {
	lang := a.lang(chatID)
	if a.store == nil {
		return
	}
	page = max(page, 0)
	list, total, err := a.store.ListReferrals(ctx, uid, referralsPageSize, page*referralsPageSize)
	if err != nil {
		a.sendHome(ctx, chatID, "❌ "+err.Error())
		return
	}
	pages := (total + referralsPageSize - 1) / referralsPageSize
	if page >= pages && pages > 0 {
		page = pages - 1
		if list, _, err = a.store.ListReferrals(ctx, uid, referralsPageSize, page*referralsPageSize); err != nil {
			a.sendHome(ctx, chatID, "❌ "+err.Error())
			return
		}
	}
	id := strconv.FormatInt(uid, 10)
	var rows [][]models.InlineKeyboardButton
	now := time.Now().UTC()
	for _, rid := range list {
		if u, _ := a.store.GetUser(ctx, rid); u != nil {
			rows = append(rows, []models.InlineKeyboardButton{userListBtn(u, a.userButtonLabel(ctx, u), now)})
			continue
		}
		rs := strconv.FormatInt(rid, 10)
		rows = append(rows, []models.InlineKeyboardButton{btn(rs, "usr:view:"+rs)})
	}
	if nav := paginationRow("usr:refs:"+id+":", page, pages, i18n.T(lang, "btn.prev"), i18n.T(lang, "btn.next")); len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []models.InlineKeyboardButton{
		btn(i18n.T(lang, "btn.back"), "usr:view:"+id), btn(i18n.T(lang, "btn.home"), "menu:home"),
	})
	a.sendUsrKB(ctx, chatID, i18n.T(lang, "user.refs_title", a.userLabelByID(ctx, uid), total), rows)
}
