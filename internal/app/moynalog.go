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
	ListIncomes(ctx context.Context, from, to time.Time) ([]moynalog.IncomeRecord, bool, error)
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

// processReceipt доводит чек до ФНС — одна попытка: pending пробивает,
// unknown сверяет по метке. Строка перечитывается под receiptBusy, поэтому
// вызывать можно с любого пути: чек, уже пробитый или занятый другим путём,
// второй раз в ФНС не уйдёт. В конце, если пора, — алерт владельцу.
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
	if r == nil {
		return
	}
	if client, _ := a.mnClientGet(); client != nil {
		switch r.State {
		case model.ReceiptPending:
			a.createIncome(ctx, client, r)
		case model.ReceiptUnknown:
			if !receiptSettled(r) {
				break
			}
			a.reconcileReceipt(ctx, client, r)
		}
		if r, err = a.store.GetReceipt(ctx, provider, extID); err != nil || r == nil {
			return
		}
	}
	a.alertReceiptIfDue(ctx, r)
}

// createIncome — одно обращение к ФНС за чеком r и запись исхода.
func (a *App) createIncome(ctx context.Context, client moyNalogAPI, r *model.Receipt) {
	incomeAt, _ := time.Parse(time.RFC3339, r.IncomeAt)

	// Запись исхода — своим контекстом: контекст обращения к ФНС мог истечь,
	// а без записи пробитый чек остался бы pending и ушёл бы в ФНС ещё раз.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer saveCancel()
	// На время обращения чек — unknown: если процесс упадёт после приёма
	// дохода ФНС, следующий проход сверит его по метке, а не пробьёт второй.
	// Не записалось — к ФНС не идём: исход потом записать тоже не выйдет.
	if err := a.store.MarkReceiptSending(saveCtx, r.Provider, r.ExtID, "обращение к ФНС не завершилось"); err != nil {
		a.log.Error("moynalog: чек не отмечен перед пробитием", "err", err, "ext", r.ExtID)
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, receiptTimeout)
	defer cancel()
	uuid, err := client.CreateIncome(callCtx, moynalog.Income{
		Amount:        float64(r.AmountKopecks) / 100,
		Name:          r.Name,
		OperationTime: incomeAt,
	})
	if err == nil {
		if serr := a.store.SetReceiptResult(saveCtx, r.Provider, r.ExtID, model.ReceiptCreated, uuid, ""); serr != nil {
			// Чек в ФНС есть, а uuid не записан. Строка осталась unknown —
			// сверка по метке найдёт этот же чек.
			a.log.Error("moynalog: чек пробит, но не записан", "err", serr, "ext", r.ExtID, "uuid", uuid)
			_ = a.store.SetReceiptResult(saveCtx, r.Provider, r.ExtID, model.ReceiptUnknown, "", "чек пробит, uuid не записался: "+uuid)
			return
		}
		a.log.Info("moynalog: чек пробит", "ext", r.ExtID, "uuid", uuid, "kopecks", r.AmountKopecks)
		return
	}
	state := receiptStateAfter(err)
	if serr := a.store.SetReceiptResult(saveCtx, r.Provider, r.ExtID, state, "", err.Error()); serr != nil {
		a.log.Error("moynalog: исход пробития не записан", "err", serr, "ext", r.ExtID, "state", state)
	}
	a.log.Error("moynalog: чек не пробит", "err", err, "ext", r.ExtID, "state", state)
}

// reconcileDelay — сколько unknown выжидает до сверки. Доход, принятый ФНС
// секунду назад, мог ещё не попасть в список доходов: сверка по такому
// списку пробила бы дубль.
const reconcileDelay = 10 * time.Minute

// receiptSettled — прошло ли с последнего исхода по чеку достаточно, чтобы
// сверять его по списку доходов.
func receiptSettled(r *model.Receipt) bool {
	at, err := time.Parse(time.RFC3339, r.UpdatedAt)
	return err != nil || time.Since(at) >= reconcileDelay
}

// reconcileWindow — запас списка доходов вокруг чека: до момента дохода и
// после текущего момента.
const reconcileWindow = 24 * time.Hour

// reconcileReceipt выясняет судьбу чека, ответ на пробитие которого потерян.
// Ищем по метке, а не по uuid: uuid выдаёт ФНС в том самом ответе, который
// потерялся. Нашли — записываем uuid. Список полный и чека нет — ФНС его не
// приняла, пробиваем заново. Список неполный или сверка не удалась — чек
// остаётся unknown до следующего прохода: пробить вслепую — рискнуть дублем.
func (a *App) reconcileReceipt(ctx context.Context, client moyNalogAPI, r *model.Receipt) {
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer saveCancel()
	keepUnknown := func(reason string) {
		if err := a.store.SetReceiptResult(saveCtx, r.Provider, r.ExtID, model.ReceiptUnknown, "", reason); err != nil {
			a.log.Error("moynalog: исход сверки не записан", "err", err, "ext", r.ExtID)
		}
	}
	if r.Marker == "" {
		a.log.Error("moynalog: чек без метки, сверять нечем", "ext", r.ExtID)
		return
	}
	incomeAt, err := time.Parse(time.RFC3339, r.IncomeAt)
	if err != nil {
		a.log.Error("moynalog: у чека нечитаемый момент дохода, сверка невозможна", "ext", r.ExtID, "income_at", r.IncomeAt)
		return
	}
	// Окно — от дохода до текущего момента: чек мог быть пробит намного позже
	// списания (пролежал в pending, пока ФНС была недоступна).
	listCtx, cancel := context.WithTimeout(ctx, receiptTimeout)
	defer cancel()
	list, complete, err := client.ListIncomes(listCtx, incomeAt.Add(-reconcileWindow), time.Now().Add(reconcileWindow))
	if err != nil {
		a.log.Warn("moynalog: сверка чека не удалась", "err", err, "ext", r.ExtID)
		keepUnknown("сверка: " + err.Error())
		return
	}
	var found []moynalog.IncomeRecord
	for _, in := range list {
		if in.Matches(r.Marker) {
			found = append(found, in)
		}
	}
	switch {
	case len(found) == 1:
		state := model.ReceiptCreated
		if found[0].Canceled {
			// Чек нашёлся аннулированным: состояние отражает кабинет, второй
			// чек по этому платежу всё равно не пробиваем.
			state = model.ReceiptCanceled
		}
		if err := a.store.SetReceiptResult(saveCtx, r.Provider, r.ExtID, state, found[0].UUID, ""); err != nil {
			a.log.Error("moynalog: найденный сверкой чек не записан", "err", err, "ext", r.ExtID, "uuid", found[0].UUID)
			return
		}
		a.log.Info("moynalog: сверка нашла чек по метке", "ext", r.ExtID, "uuid", found[0].UUID, "state", state)
	case len(found) > 1:
		// Угадывать, какой из чеков наш, автоматика не берётся: остаётся
		// unknown, через сутки владелец получит алерт и разберётся в кабинете.
		a.log.Error("moynalog: по метке нашлось несколько чеков", "ext", r.ExtID, "marker", r.Marker, "count", len(found))
		keepUnknown("по метке " + r.Marker + " нашлось чеков: " + strconv.Itoa(len(found)))
	case !complete:
		a.log.Warn("moynalog: список доходов неполный, сверка — следующим проходом", "ext", r.ExtID)
		keepUnknown("список доходов неполный, чек по метке не найден")
	default:
		a.log.Info("moynalog: сверка чек не нашла, пробиваем заново", "ext", r.ExtID, "marker", r.Marker)
		a.createIncome(ctx, client, r)
	}
}

// receiptStuckAge — сколько чек может не становиться created до алерта.
const receiptStuckAge = 24 * time.Hour

// alertReceiptIfDue шлёт владельцу алерт по чеку, если пора: отклонён ФНС
// или не пробит дольше суток. Алерт один на чек — отметка в строке ставится
// до отправки и только если её ещё нет. Отметка означает доставленный алерт:
// не принятое Telegram сообщение её снимает, и алерт повторит следующий проход.
func (a *App) alertReceiptIfDue(ctx context.Context, r *model.Receipt) {
	if r.AlertedAt != "" {
		return
	}
	lang := a.lang(a.cfg.AdminID)
	lastErr := r.LastError
	if lastErr == "" {
		lastErr = "—"
	}
	var text string
	switch r.State {
	case model.ReceiptRejected:
		text = i18n.T(lang, "admin.receipt_rejected", escapeName(r.ExtID), kopecksToRub(r.AmountKopecks), escapeName(lastErr))
	case model.ReceiptPending, model.ReceiptUnknown:
		created, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil || time.Since(created) < receiptStuckAge {
			return
		}
		text = i18n.T(lang, "admin.receipt_stuck", escapeName(r.ExtID), kopecksToRub(r.AmountKopecks), r.State, escapeName(lastErr))
	default:
		return
	}
	ok, err := a.store.MarkReceiptAlerted(ctx, r.Provider, r.ExtID)
	if err != nil {
		a.log.Error("moynalog: отметка алерта по чеку", "err", err, "ext", r.ExtID)
		return
	}
	if !ok {
		return
	}
	if a.notify(ctx, a.cfg.AdminID, text) {
		return
	}
	a.log.Error("moynalog: алерт по чеку не доставлен, повторим следующим проходом", "ext", r.ExtID)
	if err := a.store.UnmarkReceiptAlerted(ctx, r.Provider, r.ExtID); err != nil {
		a.log.Error("moynalog: снятие отметки алерта по чеку", "err", err, "ext", r.ExtID)
	}
}

const (
	// receiptPassTick — период планового прохода по чекам.
	receiptPassTick = 30 * time.Minute
	// receiptPassBatch — потолок чеков за проход: обращение к ФНС стоит до
	// минуты, а восстановление базы ждёт конца прохода. Не успевшие — в
	// следующий раз: давно не тронутые идут первыми.
	receiptPassBatch = 50
)

// RunReceipts — плановый проход по чекам раз в 30 минут.
func (a *App) RunReceipts(ctx context.Context) {
	t := time.NewTicker(receiptPassTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.receiptPassOnce(ctx)
		}
	}
}

// receiptPassOnce добивает pending, сверяет unknown и шлёт алерты. Идёт
// только по таблице чеков, а не по платежам: перенесённая из старого бота
// история платежей строк чеков не имеет и чеков не порождает.
func (a *App) receiptPassOnce(ctx context.Context) {
	if !a.enterWork() {
		return
	}
	defer a.leaveWork()
	a.mu.Lock()
	st := a.store
	a.mu.Unlock()
	if st == nil {
		return
	}
	list, err := st.ListOpenReceipts(ctx, receiptPassBatch)
	if err != nil {
		a.log.Warn("moynalog: список чеков для прохода", "err", err)
		return
	}
	for i := range list {
		if a.paused() || ctx.Err() != nil {
			return
		}
		a.processReceipt(ctx, list[i].Provider, list[i].ExtID)
	}
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
