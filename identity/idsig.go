// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"errors"
	"fmt"
)

// AgentIDSignedInputTag is the first line of the signed input of an id_sig.
// See SAP §13.10.
const AgentIDSignedInputTag = "AGENT-ID-V1"

// AgentIDSignedInput returns the bytes an id_sig covers. Note the field order:
// user first, unlike the Agent-Sender input.
//
//	AGENT-ID-V1\n{user_nkey}\n{account}\n{agent}\n{owner}\n{prompt_subject}\n
//
// promptSubject is the subject advertised by the instance's prompt endpoint,
// not one derived from the instance name. See SAP §13.10.
func AgentIDSignedInput(user, account, agent, owner, promptSubject string) []byte {
	var b bytes.Buffer
	b.WriteString(AgentIDSignedInputTag)
	for _, f := range []string{user, account, agent, owner, promptSubject} {
		b.WriteByte('\n')
		b.WriteString(f)
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// SignAgentID returns the id_sig value for registration metadata: an ed25519
// signature by the agent's user seed over [AgentIDSignedInput], base64url
// without padding. See SAP §13.10.
func SignAgentID(signer Signer, id AgentID, agent, owner, promptSubject string) (string, error) {
	if signer == nil {
		return "", errors.New("identity: SignAgentID needs a Signer")
	}
	if _, err := ParseAgentID(string(id)); err != nil {
		return "", err
	}
	if id.User() != signer.PublicKey() {
		return "", fmt.Errorf("%w: the agent ID names a different user than the signer", ErrIdentityMismatch)
	}
	sig, err := signer.Sign(AgentIDSignedInput(id.User(), id.Account(), agent, owner, promptSubject))
	if err != nil {
		return "", fmt.Errorf("identity: signing id_sig: %w", err)
	}
	return encodeBase64URL(sig), nil
}

// RegistrationMetadata returns the three keys a host with a seed adds to its
// service metadata (SAP §13.10): user_nkey, account and id_sig. After a key
// rotation, the program restarts with the new key and registers these again.
func RegistrationMetadata(signer Signer, id AgentID, agent, owner, promptSubject string) (map[string]string, error) {
	sig, err := SignAgentID(signer, id, agent, owner, promptSubject)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		MetadataKeyUserNKey: id.User(),
		MetadataKeyAccount:  id.Account(),
		MetadataKeyIDSig:    sig,
	}, nil
}

// VerifyAgentID verifies a service record's id_sig against its own prompt
// endpoint subject. It rebuilds the signed input from the user_nkey, account,
// agent and owner metadata and promptSubject, all read from the same $SRV.INFO
// record. It returns false when any field is missing, the key or signature is
// malformed, or the signature does not verify. Metadata without a verifying
// id_sig is a claim. See SAP §13.10.
func VerifyAgentID(metadata map[string]string, promptSubject string) bool {
	user, ok1 := metadata[MetadataKeyUserNKey]
	account, ok2 := metadata[MetadataKeyAccount]
	agent, ok3 := metadata["agent"]
	owner, ok4 := metadata["owner"]
	idSig, ok5 := metadata[MetadataKeyIDSig]
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return false
	}
	if validateUser(user) != nil {
		return false
	}
	sig, err := decodeSignature(idSig)
	if err != nil {
		return false
	}
	return verifyWithPublicKey(user, AgentIDSignedInput(user, account, agent, owner, promptSubject), sig)
}
