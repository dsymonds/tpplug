package tpplug

import ()

func Encrypt(data []byte) {
	// Simple scheme: each byte is XOR'd with the previous byte of ciphertext.
	prev := byte(0xAB)
	for i := range data {
		data[i] ^= prev
		prev = data[i]
	}
}

func Decrypt(data []byte) {
	prev := byte(0xAB)
	for i, b := range data {
		next := b
		data[i] ^= prev
		prev = next
	}
}
