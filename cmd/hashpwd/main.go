package main

// hashpwd — imprime el hash bcrypt (cost 10) de una password.
//
// Uso (preferido — la password NO aparece en la lista de procesos):
//
//	printf '%s' "$PASS" | go run ./cmd/hashpwd
//
// Uso legacy (visible en argv/ps — evitar con passwords reales):
//
//	go run ./cmd/hashpwd 'password'
//
// Sin argv lee la password por stdin (mismo patrón que cmd/recrypt).
// Imprime SOLO el hash por stdout; los errores van a stderr. Antes de
// imprimir verifica el hash contra la password en memoria.
import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "hashpwd: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	var pwd string
	if len(os.Args) > 1 {
		pwd = os.Args[1]
	} else {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatalf("no pude leer stdin: %v", err)
		}
		pwd = strings.TrimSpace(string(data))
	}
	if pwd == "" {
		fatalf("password vacía (pásala por stdin: printf '%%s' \"$PASS\" | go run ./cmd/hashpwd)")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pwd), 10)
	if err != nil {
		fatalf("bcrypt falló: %v", err)
	}
	// Self-test: el hash debe verificar contra la password original.
	if err := bcrypt.CompareHashAndPassword(h, []byte(pwd)); err != nil {
		fatalf("self-test falló: el hash no verifica contra la password")
	}
	fmt.Println(string(h))
}
