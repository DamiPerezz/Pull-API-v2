package main

// recrypt — re-cifra un secreto de una APP_KEY a otra (AES-256-GCM,
// formato EXACTO del backend: base64(nonce||ciphertext), nonce de 12
// bytes; reutiliza services.CryptoService, el mismo código que usan
// venue_database_configs y payment_gateway_credentials).
//
// Uso (claves de 64 hex por flag o por variable de entorno):
//
//	Re-cifrar (default) — el input es un CIPHERTEXT cifrado con OLD_KEY:
//	  printf '%s' "$CIPHERTEXT" | OLD_KEY=<64hex> NEW_KEY=<64hex> go run ./cmd/recrypt
//	  go run ./cmd/recrypt -old <64hex> -new <64hex> -in "$CIPHERTEXT"
//
//	Solo cifrar — el input es un PLAINTEXT, se cifra con NEW_KEY:
//	  printf '%s' "$SECRET" | NEW_KEY=<64hex> go run ./cmd/recrypt -encrypt
//
// Imprime SOLO el ciphertext nuevo por stdout (los diagnósticos van a
// stderr). Nunca imprime el plaintext ni las claves. Antes de imprimir
// hace un self-test en memoria: descifra el resultado con NEW_KEY y
// comprueba que coincide con el plaintext original.
import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"pull-api-v2/services"
)

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "recrypt: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	oldFlag := flag.String("old", "", "APP_KEY antigua (64 hex). Alternativa: env OLD_KEY")
	newFlag := flag.String("new", "", "APP_KEY nueva (64 hex). Alternativa: env NEW_KEY")
	inFlag := flag.String("in", "", "input (ciphertext, o plaintext con -encrypt). Si falta, se lee de stdin")
	encryptOnly := flag.Bool("encrypt", false, "modo solo-cifrar: el input es PLAINTEXT y se cifra con la clave nueva (no requiere clave antigua)")
	flag.Parse()

	oldKey := *oldFlag
	if oldKey == "" {
		oldKey = os.Getenv("OLD_KEY")
	}
	newKey := *newFlag
	if newKey == "" {
		newKey = os.Getenv("NEW_KEY")
	}
	if newKey == "" {
		fatalf("falta la clave nueva: -new <64hex> o env NEW_KEY")
	}
	if !*encryptOnly && oldKey == "" {
		fatalf("falta la clave antigua: -old <64hex> o env OLD_KEY (o usa -encrypt si el input es plaintext)")
	}

	input := *inFlag
	if input == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatalf("no pude leer stdin: %v", err)
		}
		input = string(data)
	}
	input = strings.TrimSpace(input)
	if input == "" {
		fatalf("input vacío (pásalo con -in o por stdin)")
	}

	newCrypto, err := services.NewCryptoService(newKey)
	if err != nil {
		fatalf("clave nueva inválida: %v", err)
	}

	// Self-test genérico del par cifrar→descifrar en memoria con la clave
	// nueva, ANTES de tocar el secreto real.
	const probe = "recrypt-selftest-probe"
	if enc, err := newCrypto.Encrypt(probe); err != nil {
		fatalf("self-test: fallo cifrando: %v", err)
	} else if dec, err := newCrypto.Decrypt(enc); err != nil || dec != probe {
		fatalf("self-test: el round-trip cifra/descifra no reproduce el original")
	}

	// Obtener el plaintext (descifrando con la clave antigua, o directo).
	var plaintext string
	if *encryptOnly {
		plaintext = input
	} else {
		oldCrypto, err := services.NewCryptoService(oldKey)
		if err != nil {
			fatalf("clave antigua inválida: %v", err)
		}
		plaintext, err = oldCrypto.Decrypt(input)
		if err != nil {
			fatalf("no pude descifrar el input con la clave antigua (¿clave o ciphertext equivocados?): %v", err)
		}
	}

	// Re-cifrar con la clave nueva.
	out, err := newCrypto.Encrypt(plaintext)
	if err != nil {
		fatalf("fallo cifrando con la clave nueva: %v", err)
	}

	// Self-test del resultado real: descifrar lo que vamos a imprimir y
	// comparar en memoria (jamás se imprime el plaintext).
	roundTrip, err := newCrypto.Decrypt(out)
	if err != nil || roundTrip != plaintext {
		fatalf("self-test final falló: el ciphertext nuevo no descifra al plaintext original")
	}

	fmt.Println(out)
}
