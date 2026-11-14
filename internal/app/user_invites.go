package app

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/assets"
	"remnabot/internal/i18n"
	"remnabot/internal/model"
)

// Приглашения от пользователей: в режиме «По приглашениям» с включённым
// тумблером «Приглашают пользователи» любой человек с Доступом создаёт
// одноразовые Приглашения inv_<код> для друзей. Пропуск — именно Приглашение,
// а не ref_-ссылка: см. docs/adr/0004-pass-is-invite-not-ref.md.

const (
	// userInviteTTL — срок Приглашения от пользователя.
	userInviteTTL = 30 * 24 * time.Hour
	// userInviteMaxActive — сколько неиспользованных Приглашений Автор
	// держит одновременно.
	userInviteMaxActive = 3
	// userInviteMaxPerDay — сколько Приглашений Автор создаёт за скользящие
	// сутки: отзыв и пересоздание не обходят лимит активных.
	userInviteMaxPerDay = 15
)

// cbUserInvite — кнопки экрана «Пригласить друга».
const cbUserInvite = "uinv"

// userInvitesOn — тумблер включён и действует: бот в режиме «По
// приглашениям».
func (a *App) userInvitesOn() bool {
	if a.accessMode() != model.AccessInvite {
		return false
	}
	return a.userInvitesToggle()
}

// userInvitesToggle — значение тумблера без учёта режима.
func (a *App) userInvitesToggle() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botCfg != nil && a.botCfg.UserInvites
}

// toggleUserInvites переключает тумблер владельца и сохраняет конфиг.
func (a *App) toggleUserInvites(ctx context.Context) {
	a.mu.Lock()
	if a.botCfg != nil {
		a.botCfg.UserInvites = !a.botCfg.UserInvites
	}
	a.mu.Unlock()
	_ = a.saveBotConfig(ctx)
}

// canInviteFriends — право создавать Приглашения: тумблер действует, у
// человека есть Доступ и нет блокировки. Подписка и оплаты не важны. Тем же
// условием проверяется, не стоят ли на паузе уже розданные Приглашения.
func (a *App) canInviteFriends(ctx context.Context, chatID int64) bool {
	return a.store != nil && chatID != a.cfg.AdminID && a.userInvitesOn() &&
		!a.userBlocked(ctx, chatID) && a.accessGranted(ctx, chatID)
}

// userInviteQuota — сколько у Автора активных Приглашений и сколько он создал
// за последние сутки.
func userInviteQuota(list []model.Invite, now time.Time) (active, today int) {
	dayAgo := now.Add(-24 * time.Hour)
	for i := range list {
		if list[i].Active(now) {
			active++
		}
		if t, err := time.Parse(time.RFC3339, list[i].CreatedAt); err == nil && t.After(dayAgo) {
			today++
		}
	}
	return active, today
}

// createUserInvite создаёт Приглашение от пользователя. Второе значение —
// ключ текста отказа, если создавать нельзя.
func (a *App) createUserInvite(ctx context.Context, chatID int64) (*model.Invite, string) {
	if !a.canInviteFriends(ctx, chatID) {
		return nil, "uinv.not_allowed"
	}
	a.userInviteMu.Lock()
	defer a.userInviteMu.Unlock()
	list, err := a.store.ListInvitesBy(ctx, chatID)
	if err != nil {
		a.log.Warn("user invite: список", "tg_id", chatID, "err", err)
		return nil, "uinv.fail"
	}
	now := time.Now().UTC()
	active, today := userInviteQuota(list, now)
	if active >= userInviteMaxActive {
		return nil, "uinv.limit_active"
	}
	if today >= userInviteMaxPerDay {
		return nil, "uinv.limit_day"
	}
	inv := &model.Invite{
		Code:      newInviteCode(),
		MaxUses:   1,
		ExpiresAt: now.Add(userInviteTTL).Format(time.RFC3339),
		CreatedAt: now.Format(time.RFC3339),
		CreatedBy: chatID,
		Grant:     model.InviteGrantAccess,
	}
	if err := a.store.CreateInvite(ctx, inv); err != nil {
		a.log.Warn("user invite: создание", "tg_id", chatID, "err", err)
		return nil, "uinv.fail"
	}
	a.log.Info("user invite: создано", "tg_id", chatID, "code", inv.Code)
	return inv, ""
}

// inviteCabinetLink — ссылка кабинета с Приглашением; пусто, когда кабинет
// выключен или у бота нет адреса.
func (a *App) inviteCabinetLink(code string) string {
	if !a.CabinetEnabled() {
		return ""
	}
	u := a.cabinetURL()
	if u == "" {
		return ""
	}
	return u + "?inv=" + url.QueryEscape(code)
}

// inviteShareURL — кнопка «Поделиться»: выбор чата Telegram с готовым текстом
// и обеими ссылками. Пусто, если бот не знает своего @username.
func (a *App) inviteShareURL(ctx context.Context, lang, code string) string {
	link := a.inviteLink(ctx, code)
	if link == "" {
		return ""
	}
	text := i18n.T(lang, "uinv.share_text")
	if cab := a.inviteCabinetLink(code); cab != "" {
		text += "\n" + i18n.T(lang, "uinv.share_cabinet", cab)
	}
	return "https://t.me/share/url?url=" + url.QueryEscape(link) + "&text=" + url.QueryEscape(text)
}

// userInviteBonus — блок бонусов рефералки на экране приглашений; пусто, когда
// рефералка выключена или человек бессрочный (ему бонусы не начисляются).
func (a *App) userInviteBonus(ctx context.Context, chatID int64, lang string) string {
	cfg := a.referralCfg()
	if !cfg.Enabled || a.subPerpetual(ctx, chatID) {
		return ""
	}
	bonus := i18n.T(lang, "ref.bonus_balance_n", cfg.BonusValue)
	if cfg.BonusKind == model.ReferralBonusDays {
		bonus = i18n.T(lang, "ref.bonus_days_n", cfg.BonusValue)
	}
	when := i18n.T(lang, "ref.when_pay")
	if !cfg.OnFirstPay {
		when = i18n.T(lang, "ref.when_reg")
	}
	extra := ""
	if cfg.InviteeValue > 0 && cfg.InviteeKind != "" {
		inv := i18n.T(lang, "ref.bonus_balance_n", cfg.InviteeValue)
		if cfg.InviteeKind == model.ReferralBonusDays {
			inv = i18n.T(lang, "ref.bonus_days_n", cfg.InviteeValue)
		}
		extra += "\n" + i18n.T(lang, "ref.also_friend", inv)
	}
	if cfg.Percent > 0 {
		extra += "\n" + i18n.T(lang, "ref.also_percent", cfg.Percent)
	}
	count, _ := a.store.CountReferrals(ctx, chatID)
	earned := int64(0)
	if u, _ := a.store.GetUser(ctx, chatID); u != nil {
		earned = u.RefEarned
	}
	return i18n.T(lang, "uinv.bonus", bonus, when, extra, count, kopecksToRub(earned))
}

// showUserInvites — экран «Пригласить друга». notice — строка итога действия
// над экраном.
func (a *App) showUserInvites(ctx context.Context, chatID int64, notice string) {
	lang := a.lang(chatID)
	if !a.canInviteFriends(ctx, chatID) {
		a.sendKBSection(ctx, chatID, assets.SectionReferral, i18n.T(lang, "uinv.not_allowed"),
			[][]models.InlineKeyboardButton{backHomeRow(lang)})
		return
	}
	list, err := a.store.ListInvitesBy(ctx, chatID)
	if err != nil {
		a.log.Warn("user invite: список", "tg_id", chatID, "err", err)
		a.sendKBSection(ctx, chatID, assets.SectionReferral, i18n.T(lang, "uinv.fail"),
			[][]models.InlineKeyboardButton{backHomeRow(lang)})
		return
	}
	now := time.Now().UTC()
	active, _ := userInviteQuota(list, now)
	var b strings.Builder
	if notice != "" {
		b.WriteString(notice)
		b.WriteString("\n\n")
	}
	b.WriteString(i18n.T(lang, "uinv.title"))
	if bonus := a.userInviteBonus(ctx, chatID, lang); bonus != "" {
		b.WriteString("\n\n")
		b.WriteString(bonus)
	}
	b.WriteString("\n\n")
	b.WriteString(i18n.T(lang, "uinv.rules", int(userInviteTTL/(24*time.Hour)), active, userInviteMaxActive))
	var rows [][]models.InlineKeyboardButton
	n := 0
	for i := range list {
		inv := &list[i]
		if !inv.Active(now) {
			continue
		}
		n++
		link := a.inviteLink(ctx, inv.Code)
		if link == "" {
			link = i18n.T(lang, "inv.no_link", inv.Code)
		}
		b.WriteString("\n\n")
		b.WriteString(i18n.T(lang, "uinv.item", n, link, formatExpire(inv.ExpiresAt, lang)))
		var row []models.InlineKeyboardButton
		if share := a.inviteShareURL(ctx, lang, inv.Code); share != "" {
			row = append(row, models.InlineKeyboardButton{Text: i18n.T(lang, "uinv.btn_share", n), URL: share})
		}
		row = append(row, btn(i18n.T(lang, "uinv.btn_revoke", n), cbUserInvite+":revoke:"+inv.Code))
		rows = append(rows, row)
	}
	if active < userInviteMaxActive {
		rows = append(rows, []models.InlineKeyboardButton{btn(i18n.T(lang, "uinv.btn_new"), cbUserInvite+":new")})
	}
	rows = append(rows, backHomeRow(lang))
	a.sendKBSection(ctx, chatID, assets.SectionReferral, b.String(), rows)
}

// onUserInvite — кнопки экрана «Пригласить друга».
func (a *App) onUserInvite(ctx context.Context, chatID int64, val string) {
	lang := a.lang(chatID)
	action, arg, _ := strings.Cut(val, ":")
	switch action {
	case "new":
		if _, reason := a.createUserInvite(ctx, chatID); reason != "" {
			var args []any
			switch reason {
			case "uinv.limit_active":
				args = []any{userInviteMaxActive}
			case "uinv.limit_day":
				args = []any{userInviteMaxPerDay}
			}
			a.showUserInvites(ctx, chatID, i18n.T(lang, reason, args...))
			return
		}
		a.showUserInvites(ctx, chatID, i18n.T(lang, "uinv.created"))
	case "revoke":
		notice := i18n.T(lang, "uinv.revoke_none")
		if a.store != nil && arg != "" && a.canInviteFriends(ctx, chatID) {
			ok, err := a.store.RevokeInviteBy(ctx, arg, chatID)
			if err != nil {
				a.log.Warn("user invite: отзыв", "tg_id", chatID, "code", arg, "err", err)
			}
			if ok {
				notice = i18n.T(lang, "uinv.revoked")
			}
		}
		a.showUserInvites(ctx, chatID, notice)
	default:
		a.showUserInvites(ctx, chatID, "")
	}
}

// notifyUserInviteUsed — уведомления об активации Приглашения от
// пользователя: Автору и владельцу.
func (a *App) notifyUserInviteUsed(ctx context.Context, author, friend int64) {
	// Автору — только имя друга, без Telegram ID; владельцу — подписи с ID.
	alang := a.lang(author)
	first, username := "", ""
	if u, _ := a.store.GetUser(ctx, friend); u != nil {
		first, username = u.FirstName, u.Username
	}
	a.notify(ctx, author, i18n.T(alang, "uinv.used_author", displayName(alang, first, username)))
	olang := a.lang(a.cfg.AdminID)
	a.notify(ctx, a.cfg.AdminID, i18n.T(olang, "uinv.used_admin", a.userLabelByID(ctx, author), a.userLabelByID(ctx, friend)))
}

// refPayload — payload ссылки рефералки для Автора: привязка Пригласившего
// идёт через ту же логику, что и ref_<id>.
func refPayload(author int64) string {
	return "ref_" + strconv.FormatInt(author, 10)
}
