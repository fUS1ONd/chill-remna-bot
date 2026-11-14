package app

import (
	"context"
	"crypto/rand"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"remnabot/internal/assets"
	"remnabot/internal/i18n"
	"remnabot/internal/model"
	"remnabot/internal/moynalog"
)

func (a *App) moynalogCfg() model.MoyNalogConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.botCfg == nil {
		return model.MoyNalogConfig{}
	}
	return a.botCfg.MoyNalog
}

// moyNalogAPI — кабинет «Мой налог», как его видит бот. Шов для фейка ФНС в
// тестах.
type moyNalogAPI interface {
	CreateIncome(ctx context.Context, in moynalog.Income) (string, error)
}

func (a *App) mnClientGet() (moyNalogAPI, model.MoyNalogConfig) {
	cfg := a.moynalogCfg()
	if !cfg.Enabled || cfg.Login == "" || cfg.Password == "" {
		return nil, cfg
	}
	a.mnMu.Lock()
	defer a.mnMu.Unlock()
	key := cfg.Login + "|" + cfg.Password
	if a.mnClient == nil || a.mnKey != key {
		if a.mnNew != nil {
			a.mnClient = a.mnNew(cfg.Login, cfg.Password)
		} else {
			a.mnClient = moynalog.New(cfg.Login, cfg.Password)
		}
		a.mnKey = key
	}
	return a.mnClient, cfg
}

// receiptTimeout — потолок одного обращения к ФНС вместе со входом в кабинет.
const receiptTimeout = time.Minute

// issueReceipt заводит чек «Мой налог» по оплате и пробивает его в фоне.
//
// Чек положен только деньгам, пришедшим через ЮKassa: оплата с баланса учтена
// при пополнении, прочие провайдеры в чек самозанятого не идут. Право на чек
// столбится строкой в базе здесь же, синхронно и до ФНС: не вставилась —
// чек по этому платежу уже чей-то, второго не будет. Сам поход в ФНС идёт в
// фоне: вне замков финализации и вне ответа вебхуку.
func (a *App) issueReceipt(ctx context.Context, provider, extID string, kopecks int64, detail string, incomeAt time.Time) {
	if provider != model.PayMethodYooKassa || a.store == nil || kopecks <= 0 {
		return
	}
	client, cfg := a.mnClientGet()
	if client == nil {
		return
	}
	if extID == "" {
		a.log.Error("moynalog: платёж без id, чек не застолбить", "provider", provider, "kopecks", kopecks)
		return
	}
	marker, err := newReceiptMarker()
	if err != nil {
		a.log.Error("moynalog: метка чека", "err", err)
		return
	}
	name := detail
	if cfg.ServiceName != "" {
		name = cfg.ServiceName
	}
	r := &model.Receipt{
		Provider:      provider,
		ExtID:         extID,
		AmountKopecks: kopecks,
		// Метка в наименовании — единственное, по чему чек находится в списке
		// доходов ФНС, если ответ на пробитие потерялся.
		Name:     name + " (" + marker + ")",
		Marker:   marker,
		IncomeAt: incomeAt.UTC().Format(time.RFC3339),
	}
	claimed, err := a.store.ClaimReceipt(ctx, r)
	if err != nil {
		a.log.Error("moynalog: чек не застолблён", "err", err, "provider", provider, "ext", extID)
		a.notify(ctx, a.cfg.AdminID, i18n.T(a.lang(a.cfg.AdminID), "admin.receipt_unclaimed",
			escapeName(extID), kopecksToRub(kopecks)))
		return
	}
	if !claimed {
		a.log.Info("moynalog: чек по платежу уже застолблён", "provider", provider, "ext", extID)
		return
	}
	a.trackMoney("receipt", func(ctx context.Context) {
		a.processReceipt(ctx, provider, extID)
	})
}

// incomeAt — момент дохода для чека: списание денег по данным провайдера, а
// если провайдер его не сообщил — момент принятия платежа ботом. Бот мог
// узнать об оплате позже списания (вебхук потерялся, платёж подхватила
// сверка), а на стыке месяцев это уже другой налоговый период.
func incomeAt(capturedAt time.Time) time.Time {
	if capturedAt.IsZero() {
		return time.Now()
	}
	return capturedAt
}

// capturedStr — момент списания для записи платежа: RFC3339 в UTC, "" —
// неизвестен.
func capturedStr(capturedAt time.Time) string {
	if capturedAt.IsZero() {
		return ""
	}
	return capturedAt.UTC().Format(time.RFC3339)
}

func receiptKey(provider, extID string) string { return provider + "/" + extID }

// processReceipt доводит застолблённый чек в pending до ФНС — одна попытка.
// Строка перечитывается под receiptBusy, поэтому вызывать можно с любого пути:
// чек, уже пробитый или занятый другим путём, второй раз в ФНС не уйдёт.
func (a *App) processReceipt(ctx context.Context, provider, extID string) {
	key := receiptKey(provider, extID)
	if _, busy := a.receiptBusy.LoadOrStore(key, struct{}{}); busy {
		return
	}
	defer a.receiptBusy.Delete(key)

	r, err := a.store.GetReceipt(ctx, provider, extID)
	if err != nil {
		a.log.Error("moynalog: чтение чека", "err", err, "ext", extID)
		return
	}
	if r == nil || r.State != model.ReceiptPending {
		return
	}
	client, _ := a.mnClientGet()
	if client == nil {
		return
	}
	incomeAt, _ := time.Parse(time.RFC3339, r.IncomeAt)

	ctx, cancel := context.WithTimeout(ctx, receiptTimeout)
	defer cancel()
	uuid, err := client.CreateIncome(ctx, moynalog.Income{
		Amount:        float64(r.AmountKopecks) / 100,
		Name:          r.Name,
		OperationTime: incomeAt,
	})
	// Запись исхода — своим контекстом: контекст обращения к ФНС мог истечь,
	// а без записи пробитый чек остался бы pending и ушёл бы в ФНС ещё раз.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer saveCancel()
	if err == nil {
		if serr := a.store.SetReceiptResult(saveCtx, provider, extID, model.ReceiptCreated, uuid, ""); serr != nil {
			// Чек в ФНС есть, а в базе он pending — следующий проход пробил бы
			// второй. Уводим в unknown: сверка по метке найдёт этот же чек.
			a.log.Error("moynalog: чек пробит, но не записан", "err", serr, "ext", extID, "uuid", uuid)
			_ = a.store.SetReceiptResult(saveCtx, provider, extID, model.ReceiptUnknown, "", "чек пробит, uuid не записался: "+uuid)
			return
		}
		a.log.Info("moynalog: чек пробит", "ext", extID, "uuid", uuid, "kopecks", r.AmountKopecks)
		return
	}
	state := receiptStateAfter(err)
	if serr := a.store.SetReceiptResult(saveCtx, provider, extID, state, "", err.Error()); serr != nil {
		a.log.Error("moynalog: исход пробития не записан", "err", serr, "ext", extID, "state", state)
	}
	a.log.Error("moynalog: чек не пробит", "err", err, "ext", extID, "state", state)
}

// receiptStateAfter — состояние чека после неудачного обращения к ФНС.
func receiptStateAfter(err error) string {
	switch moynalog.KindOf(err) {
	case moynalog.KindUnknown:
		return model.ReceiptUnknown
	case moynalog.KindRejected:
		return model.ReceiptRejected
	default:
		return model.ReceiptPending
	}
}

// newReceiptMarker — случайная метка чека из шести символов a-z0-9. Не номер
// платежа: порядковый номер выдал бы налоговой число расчётов.
func newReceiptMarker() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf), nil
}

func parseAmountRub(s string) float64 {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.ReplaceAll(f[0], ",", "."), 64)
	return v
}

// parseAmountRubOnly возвращает сумму из строки платежа и признак, что валюта
// действительно рублёвая (₽/RUB/RUR или не указана). Tribute присылает и
// EUR/USD, у Stars валюта «⭐» — считать такие числа рублями нельзя ни в
// проценте рефереру, ни в выручке.
func parseAmountRubOnly(s string) (float64, bool) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(f[0], ",", "."), 64)
	if err != nil {
		return 0, false
	}
	if len(f) == 1 {
		return v, true
	}
	switch strings.ToUpper(f[1]) {
	case "₽", "RUB", "RUR":
		return v, true
	}
	return v, false
}

func (a *App) showMoyNalogAdmin(ctx context.Context, chatID int64) {
	lang := a.lang(chatID)
	cfg := a.moynalogCfg()
	mark := "❌"
	if cfg.Enabled {
		mark = "✅"
	}
	login := i18n.T(lang, "mn.not_set")
	if cfg.Login != "" {
		login = cfg.Login
	}
	pass := i18n.T(lang, "mn.not_set")
	if cfg.Password != "" {
		pass = "•••••"
	}
	name := cfg.ServiceName
	if name == "" {
		name = i18n.T(lang, "mn.default_name")
	}
	text := i18n.T(lang, "mn.title", mark, login, pass, name)
	a.sendKBSection(ctx, chatID, assets.SectionBuySubscription, text, [][]models.InlineKeyboardButton{
		{toggleBtn(lang, cfg.Enabled, "mn:toggle")},
		{btn(i18n.T(lang, "mn.btn_login"), "mn:login"), btn(i18n.T(lang, "mn.btn_pass"), "mn:pass")},
		{btn(i18n.T(lang, "mn.btn_name"), "mn:name")},
		navBack(lang, "menu:pay"),
	})
}

func (a *App) onMoyNalogAdmin(ctx context.Context, chatID int64, val string) {
	lang := a.lang(chatID)
	switch val {
	case "toggle":
		a.mu.Lock()
		if a.botCfg != nil {
			a.botCfg.MoyNalog.Enabled = !a.botCfg.MoyNalog.Enabled
		}
		a.mu.Unlock()
		_ = a.saveBotConfig(ctx)
		a.showMoyNalogAdmin(ctx, chatID)
	case "login":
		a.getUI(chatID).adminInput = "mn_login"
		a.askInput(ctx, chatID, i18n.T(lang, "mn.ask_login"), "menu:moynalog")
	case "pass":
		a.getUI(chatID).adminInput = "mn_pass"
		a.askInput(ctx, chatID, i18n.T(lang, "mn.ask_pass"), "menu:moynalog")
	case "name":
		a.getUI(chatID).adminInput = "mn_name"
		a.askInput(ctx, chatID, i18n.T(lang, "mn.ask_name"), "menu:moynalog")
	}
}

func (a *App) setMoyNalogField(ctx context.Context, chatID int64, field, text string) {
	text = strings.TrimSpace(text)
	a.mu.Lock()
	if a.botCfg != nil {
		switch field {
		case "mn_login":
			a.botCfg.MoyNalog.Login = text
		case "mn_pass":
			a.botCfg.MoyNalog.Password = text
		case "mn_name":
			a.botCfg.MoyNalog.ServiceName = text
		}
	}
	a.mu.Unlock()
	a.mnMu.Lock()
	a.mnClient = nil
	a.mnKey = ""
	a.mnMu.Unlock()
	_ = a.saveBotConfig(ctx)
	a.showMoyNalogAdmin(ctx, chatID)
}
