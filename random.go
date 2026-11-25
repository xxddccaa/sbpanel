package main

import "crypto/rand"

func randomPassword(length int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	for index, value := range raw {
		raw[index] = alphabet[int(value)%len(alphabet)]
	}
	return string(raw)
}
