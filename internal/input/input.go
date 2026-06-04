package input

// GamepadState é o estado de um controle Xbox enviado pelo client a cada tick.
// O client serializa os botões como um array de bools na ordem padrão da
// Gamepad API (mapeamento "standard"), e os eixos como floats.
type GamepadState struct {
	// Botões na ordem standard da Gamepad API:
	// [A,B,X,Y, LB,RB, LT,RT, Back,Start, LStick,RStick, DUp,DDown,DLeft,DRight, Guide]
	Buttons []bool `json:"buttons"`

	// Eixos: [LX, LY, RX, RY] no range -1.0..1.0
	Axes []float64 `json:"axes"`
}

type Command struct {
	Type   string  `json:"type"`
	Code   string  `json:"code,omitempty"`
	Down   bool    `json:"down,omitempty"`
	Button int     `json:"button,omitempty"`
	DX     int32   `json:"dx,omitempty"`
	DY     int32   `json:"dy,omitempty"`
	DeltaY float64 `json:"deltaY,omitempty"`
}

// Índices dos botões no array Buttons (Gamepad API standard mapping).
const (
	BtnA = iota
	BtnB
	BtnX
	BtnY
	BtnLB
	BtnRB
	BtnLT // analógico tratado como botão pela API; valor em separado se necessário
	BtnRT
	BtnBack
	BtnStart
	BtnLStick
	BtnRStick
	BtnDUp
	BtnDDown
	BtnDLeft
	BtnDRight
	BtnGuide
)

// Injector recebe estados de gamepad e os aplica num controle virtual no host.
type Injector interface {
	Apply(state GamepadState) error
	Command(cmd Command) error
	Close() error
}

func pressed(s GamepadState, idx int) bool {
	if idx < 0 || idx >= len(s.Buttons) {
		return false
	}
	return s.Buttons[idx]
}

func axis(s GamepadState, idx int) float64 {
	if idx < 0 || idx >= len(s.Axes) {
		return 0
	}
	return s.Axes[idx]
}
