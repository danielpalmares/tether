package steam

import (
	"fmt"
	"os/exec"
	"runtime"
)

// LaunchBigPicture abre o Steam no modo Big Picture.
// Usa o protocolo steam:// que o cliente Steam registra no Windows.
func LaunchBigPicture() error {
	switch runtime.GOOS {
	case "windows":
		// "cmd /c start" resolve o handler de protocolo registrado.
		cmd := exec.Command("cmd", "/c", "start", "", "steam://open/bigpicture")
		return cmd.Start()
	case "linux":
		cmd := exec.Command("xdg-open", "steam://open/bigpicture")
		return cmd.Start()
	default:
		return fmt.Errorf("SO não suportado para abrir o Big Picture: %s", runtime.GOOS)
	}
}
