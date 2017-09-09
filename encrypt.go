package main

import (
	"github.com/thecloakproject/utils/crypt"
)

type Buffer []byte

func (buf *Buffer) Write(p []byte) (int, error) {
	*buf = append(*buf, p...)
	return len(p), nil
}

func encryptEmailBody(from, to, body string) (enc []byte, err error) {
	var buf Buffer
	err = crypt.EncryptMessage(&buf, from, to, body)
	if err != nil {
		return nil, err
	}
	return []byte(buf), nil
}
