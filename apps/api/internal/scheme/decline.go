package scheme

// Decline codes.
//
// Every decline a terminal shows a cashier comes from this list. They are
// deliberately separate from the internal reasons in tapcrypto: a cardholder
// standing at a counter must not be told that their credential failed a CMAC
// check, and support must not have to guess which of nine internal states
// produced "declined".
//
// The numeric values follow ISO 8583 DE39 conventions so that a member bank
// integrating later finds codes it already understands.
type DeclineCode string

const (
	DeclineNone              DeclineCode = ""
	DeclineInvalidCard       DeclineCode = "14" // no such card
	DeclineExpiredCard       DeclineCode = "54"
	DeclineInsufficientFunds DeclineCode = "51"
	DeclineExceedsLimit      DeclineCode = "61" // over the per-transaction cap
	DeclineVelocityExceeded  DeclineCode = "65" // over the daily cap or count
	DeclineRestrictedCard    DeclineCode = "62" // frozen, or a suspected clone
	DeclineSuspectedFraud    DeclineCode = "59"
	DeclineInvalidTerminal   DeclineCode = "58"
	DeclineFormatError       DeclineCode = "30"
	DeclineSystemError       DeclineCode = "96"
)

// Message is what the terminal prints. Short, honest, and free of anything that
// would help someone probing the network map its internals.
func (d DeclineCode) Message() string {
	switch d {
	case DeclineInvalidCard:
		return "Card not recognised"
	case DeclineExpiredCard:
		return "Card expired"
	case DeclineInsufficientFunds:
		return "Insufficient funds"
	case DeclineExceedsLimit:
		return "Amount exceeds card limit"
	case DeclineVelocityExceeded:
		return "Daily limit reached"
	case DeclineRestrictedCard:
		return "Card restricted — contact issuer"
	case DeclineSuspectedFraud:
		return "Declined — contact issuer"
	case DeclineInvalidTerminal:
		return "Terminal not recognised"
	case DeclineFormatError:
		return "Could not read card"
	default:
		return "Declined"
	}
}
