/*
 * Copyright (C) 2018 The ontology Authors
 * This file is part of The ontology library.
 *
 * The ontology is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The ontology is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Lesser General Public License for more details.
 *
 * You should have received a copy of the GNU Lesser General Public License
 * along with The ontology.  If not, see <http://www.gnu.org/licenses/>.
 */

package keypair

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"

	"github.com/ontio/ontology-crypto/ec"

	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/scrypt"
)

// ProtectedKey stores the encrypted private key and related data
type ProtectedKey struct {
	Address string            `json:"address"`
	EncAlg  string            `json:"enc-alg"`
	Key     []byte            `json:"key"`
	Hash    string            `json:"hash"`
	Alg     string            `json:"algorithm"`
	Param   map[string]string `json:"parameters,omitempty"`
}

// ScryptParam contains the parameters used in scrypt function
type ScryptParam struct {
	P     int `json:"p"`
	N     int `json:"n"`
	R     int `json:"r"`
	DKLen int `json:"dkLen,omitempty"`
}

const (
	DEFAULT_N                  = 16384
	DEFAULT_R                  = 8
	DEFAULT_P                  = 8
	DEFAULT_DERIVED_KEY_LENGTH = 64
)

// Encrypt the private key with the given password.
// The password is used to derive a key via scrypt function.
// AES with CTR mode is used for encryption. The first 16 bytes of the derived
// key is used as the initial vector (IV), and the last 32 bytes is used as the
// encryption key.
func EncryptPrivateKey(pri PrivateKey, addr string, pwd []byte) (*ProtectedKey, error) {
	return EncryptWithCustomScrypt(pri, addr, pwd, GetScryptParameters())
}

// Decrypt the private key using the given password
func DecryptPrivateKey(prot *ProtectedKey, pwd []byte) (PrivateKey, error) {
	return DecryptWithCustomScrypt(prot, pwd, GetScryptParameters())
}

func EncryptWithCustomScrypt(pri PrivateKey, addr string, pwd []byte, param *ScryptParam) (*ProtectedKey, error) {
	var res = ProtectedKey{
		Address: addr,
		Hash:    "sha256",
		EncAlg:  "aes-256-ctr",
	}

	iv, ekey, err := kdf(addr, pwd, param)
	if err != nil {
		return nil, NewEncryptError(err.Error())
	}

	// Prepare the private key data for encryption
	var plaintext []byte
	switch t := pri.(type) {
	case *ec.PrivateKey:
		plaintext = t.D.Bytes()
		switch t.Algorithm {
		case ec.ECDSA:
			res.Alg = "ECDSA"
		case ec.SM2:
			res.Alg = "SM2"
		default:
			panic("unsupported ec algorithm")
		}
		res.Param = make(map[string]string)
		res.Param["curve"] = t.Params().Name
	case ed25519.PrivateKey:
		plaintext = []byte(t)
		res.Alg = "Ed25519"
	default:
		panic("unsupported key type")
	}

	ciphertext, err := ctrCipher(plaintext, ekey, iv)
	if err != nil {
		return nil, NewEncryptError(err.Error())
	}
	res.Key = ciphertext

	return &res, nil
}

func DecryptWithCustomScrypt(prot *ProtectedKey, pwd []byte, param *ScryptParam) (PrivateKey, error) {
	if prot == nil || len(pwd) == 0 {
		return nil, NewDecryptError("invalid argument")
	}

	// Check parameters
	if prot.EncAlg != "aes-256-ctr" {
		return nil, NewDecryptError("unsupported encryption algorithm")
	}

	// Derive key
	iv, ekey, err := kdf(prot.Address, pwd, param)
	if err != nil {
		return nil, NewDecryptError(err.Error())
	}

	// Decryption, same process as encryption
	plaintext, err := ctrCipher(prot.Key, ekey, iv)
	if err != nil {
		return nil, NewDecryptError(err.Error())
	}

	switch prot.Alg {
	case "ECDSA", "SM2":
		curve, err := GetNamedCurve(prot.Param["curve"])
		if err != nil {
			return nil, NewDecryptError(err.Error())
		}
		pri := ec.PrivateKey{PrivateKey: ec.ConstructPrivateKey(plaintext, curve)}
		if prot.Alg == "ECDSA" {
			pri.Algorithm = ec.ECDSA
		} else if prot.Alg == "SM2" {
			pri.Algorithm = ec.SM2
		} else {
			return nil, NewDecryptError("unknown ec algorithm")
		}
		return &pri, nil
	case "Ed25519":
		if len(plaintext) != ed25519.PrivateKeySize {
			return nil, NewDecryptError("invalid Ed25519 private key length")
		}
		return ed25519.PrivateKey(plaintext), nil
	default:
		return nil, NewDecryptError("unknown key type")
	}
}

// Re-encrypt the private key with the new password and scrypt parameters.
// The old password and scrypt parameters are used for decryption first.
// The scrypt parameters will be reseted to the default after this function.
func ReencryptPrivateKey(prot *ProtectedKey, oldPwd, newPwd []byte, oldParam, newParam *ScryptParam) (*ProtectedKey, error) {
	pri, err := DecryptWithCustomScrypt(prot, oldPwd, oldParam)
	if err != nil {
		return nil, err
	}
	newProt, err := EncryptWithCustomScrypt(pri, prot.Address, newPwd, newParam)
	return newProt, err
}

func kdf(addr string, pwd []byte, param *ScryptParam) (iv, key []byte, err error) {
	if param.DKLen < 32 {
		err = errors.New("derived key length too short")
		return
	}

	// Hash the address twice to get the salt
	digest := sha256.Sum256([]byte(addr))
	digest = sha256.Sum256(digest[:])
	// Derive the encryption key
	dk, err0 := scrypt.Key([]byte(pwd), digest[:4], param.N, param.R, param.P, param.DKLen)
	if err0 != nil {
		err = err0
		return
	}

	iv = dk[:16]
	key = dk[len(dk)-32:]
	return
}

func ctrCipher(data, key, iv []byte) ([]byte, error) {
	// AES encryption
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	ciphertext := make([]byte, len(data))
	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(ciphertext, data)
	return ciphertext, nil
}

// Return the default parameters used in scrypt function
func GetScryptParameters() *ScryptParam {
	return &ScryptParam{
		N:     DEFAULT_N,
		R:     DEFAULT_R,
		P:     DEFAULT_P,
		DKLen: DEFAULT_DERIVED_KEY_LENGTH,
	}
}
