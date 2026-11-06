package main

import "encoding/base64"

func b64enc(b []byte) string      { return base64.StdEncoding.EncodeToString(b) }
func b64dec(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
