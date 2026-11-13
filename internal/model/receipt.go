package model

// Состояния чека «Мой налог».
const (
	// ReceiptPending — право на чек застолблено, в ФНС он ещё не пробит.
	ReceiptPending = "pending"
	// ReceiptCreated — ФНС приняла доход и вернула uuid чека.
	ReceiptCreated = "created"
	// ReceiptUnknown — ответ ФНС потерян или нечитаем: чек мог создаться.
	// Пробивать заново вслепую нельзя — сначала сверка по метке.
	ReceiptUnknown = "unknown"
	// ReceiptRejected — ФНС отказала окончательно. Терминальное состояние.
	ReceiptRejected = "rejected"
	// ReceiptCanceled — чек аннулирован.
	ReceiptCanceled = "canceled"
)

// Receipt — чек «Мой налог» по одному платежу провайдера. Ключ — пара
// Provider + ExtID: он есть, даже если строка платежа у нас не записалась.
type Receipt struct {
	Provider string
	ExtID    string
	// AmountKopecks — сумма дохода.
	AmountKopecks int64
	// Name — наименование услуги ровно в том виде, в каком оно уходит в ФНС,
	// вместе с меткой.
	Name string
	// Marker — случайная метка в наименовании: по ней чек находится в списке
	// доходов ФНС, если ответ на пробитие потерялся.
	Marker string
	// IncomeAt — момент дохода (RFC3339, UTC), им датируется чек.
	IncomeAt  string
	UUID      string
	State     string
	Attempts  int
	LastError string
	// AlertedAt — когда владельцу ушёл алерт по этому чеку ("" — не уходил).
	AlertedAt string
	CreatedAt string
	UpdatedAt string
}
