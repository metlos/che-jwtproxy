// Copyright 2016 CoreOS, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package autogenerated

import (
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/go-oidc/key"
	jose "gopkg.in/square/go-jose.v2"
	"gopkg.in/yaml.v2"

	"github.com/eclipse/che-jwtproxy/config"
	"github.com/eclipse/che-jwtproxy/jwt/keyserver"
	"github.com/eclipse/che-jwtproxy/jwt/privatekey"
)

func init() {
	privatekey.Register("autogenerated", constructor)
}

type Autogenerated struct {
	active  *key.PrivateKey
	pending *key.PrivateKey
	manager keyserver.Manager
	keyLock sync.Mutex
	stopCh  chan struct{}
	doneCh  chan struct{}
	keyPath string
}

type Config struct {
	RotationInterval time.Duration                     `yaml:"rotate_every"`
	KeyServer        config.RegistrableComponentConfig `yaml:"key_server"`
	KeyFolder        string                            `yaml:"key_folder"`
}

func constructor(registrableComponentConfig config.RegistrableComponentConfig, signerParams config.SignerParams) (privatekey.PrivateKey, error) {
	cfg := Config{
		RotationInterval: 12 * time.Hour,
	}
	bytes, err := yaml.Marshal(registrableComponentConfig.Options)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(bytes, &cfg)
	if err != nil {
		return nil, err
	}

	manager, err := keyserver.NewManager(cfg.KeyServer, signerParams)
	if err != nil {
		return nil, err
	}

	privateKeyPath := keyPath(cfg.KeyFolder, signerParams.Issuer)

	var activeKey *key.PrivateKey

	// Load the private key, or leave nil if there was no stored PK.
	storedPrivateKey, err := loadPrivateKey(privateKeyPath)
	if err == nil {
		// Let's verify the key we found on disk.
		err := manager.VerifyPublicKey(storedPrivateKey.ID())
		if err == nil {
			// We verified the key, nothing more to do
			log.Debug("Successfully loaded and verified private key at path: ", privateKeyPath)
			activeKey = storedPrivateKey
		} else {
			switch err {
			case keyserver.ErrPublicKeyNotFound:
				log.Debug("Public Key not found - generating a new key")
			case keyserver.ErrPublicKeyExpired:
				log.WithError(err).Fatal("Public key has expired; delete or renew it.")
			default:
				log.WithError(err).Fatal(err.Error())
			}
		}
	} else {
		log.Debug("Unable to load private key: ", err)
	}

	ag := &Autogenerated{
		active:  activeKey,
		pending: nil,
		manager: manager,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		keyPath: privateKeyPath,
	}

	publicationResult := keyserver.NewPublishResult()
	if activeKey == nil {
		log.Debug("Boostrapping publication with a new key")
		publicationResult = ag.attemptPublish(nil, cfg.RotationInterval)
	}

	go ag.publishAndRotate(cfg.RotationInterval, publicationResult)

	return ag, nil
}

func (ag *Autogenerated) GetPrivateKey() (*key.PrivateKey, error) {
	ag.keyLock.Lock()
	defer ag.keyLock.Unlock()

	if ag.active == nil {
		return nil, errors.New("No key is yet active")
	}
	return ag.active, nil
}

func (ag *Autogenerated) Stop() <-chan struct{} {
	close(ag.stopCh)

	ag.keyLock.Lock()
	defer ag.keyLock.Unlock()

	if ag.pending != nil {
		ag.revokeKey(ag.pending)
	}

	return ag.doneCh
}

func keyPath(basePath, issuer string) string {
	if basePath == "" {
		configPath := os.Getenv("XDG_CONFIG_HOME")
		if configPath == "" {
			configPath = path.Join(os.Getenv("HOME"), ".config")
		}
		basePath = path.Join(configPath, "jwtproxy")
	}
	return path.Join(basePath, fmt.Sprintf("%s.jwk", issuer))
}

func loadPrivateKey(keyPath string) (*key.PrivateKey, error) {
	pkContents, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	jwk := jose.JSONWebKey{}
	if err := jwk.UnmarshalJSON(pkContents); err != nil {
		return nil, err
	}

	pk := key.PrivateKey{
		KeyID:      jwk.KeyID,
		PrivateKey: jwk.Key.(*rsa.PrivateKey),
	}
	if err := pk.PrivateKey.Validate(); err != nil {
		return nil, err
	}

	pk.PrivateKey.Precompute()

	return &pk, nil
}

func savePrivateKey(key *key.PrivateKey, keyPath string) error {
	err := os.MkdirAll(path.Dir(keyPath), os.ModeDir|0755)
	if err != nil {
		log.Warn("Unable to create private key file directory: ", err)
		return err
	}

	pkFile, err := os.OpenFile(keyPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Warn("Unable to open private key file to save: ", err)
		return err
	}
	defer pkFile.Close()

	jwk := jose.JSONWebKey{
		Key:       key.PrivateKey,
		KeyID:     key.KeyID,
		Algorithm: "rsa",
		Use:       "",
	}

	jwkJson, err := jwk.MarshalJSON()
	if err != nil {
		log.Warn("Unable to encode private key: ", err)
		return err
	}

	pkFile.Write(jwkJson)
	log.Debug("Successfully saved private key to: ", keyPath)
	return nil
}

// Attempt to publish a new key, if the signing key is nil we will self-sign
// the key.
func (ag *Autogenerated) attemptPublish(signingKey *key.PrivateKey, rotateInterval time.Duration) *keyserver.PublishResult {
	// We want to do this outside of the lock since it may take some time.
	candidate, err := key.GeneratePrivateKey()

	candidateJwk := jose.JSONWebKey{
		Key:       candidate.PrivateKey,
		KeyID:     candidate.KeyID,
		Algorithm: "rsa",
		Use:       "",
	}
	thumbprint, err := candidateJwk.Thumbprint(crypto.SHA256)
	candidate.KeyID = base64.URLEncoding.EncodeToString(thumbprint)

	if err != nil {
		immediateResult := keyserver.NewPublishResult()
		immediateResult.SetError(fmt.Errorf("Unable to generate new key: %s", err))
		return immediateResult
	}

	if ag.pending != nil {
		log.Debug("Best effort revoking unapproved key due to rotation")
		go ag.revokeKey(ag.pending)
	}

	ag.keyLock.Lock()
	defer ag.keyLock.Unlock()

	ag.pending = candidate
	pendingPublic := key.NewPublicKey(ag.pending.JWK())

	if signingKey == nil {
		signingKey = ag.pending
	}

	policy := &keyserver.KeyPolicy{}
	if rotateInterval > 0 {
		log.Debug("Adding rotation policy: ", rotateInterval)
		expirationTime := time.Now().Add(rotateInterval * 2)
		policy.Expiration = &expirationTime
		policy.RotationPolicy = &rotateInterval
	}

	return ag.manager.PublishPublicKey(pendingPublic, policy, signingKey)
}

// Caller MUST NOT hold the ag.keyLock.
func (ag *Autogenerated) getLogger() *log.Entry {
	ag.keyLock.Lock()
	defer ag.keyLock.Unlock()

	var activeKeyID interface{}
	if ag.active != nil {
		activeKeyID = ag.active.ID()[0:10]
	}

	var pendingKeyID interface{}
	if ag.pending != nil {
		pendingKeyID = ag.pending.ID()[0:10]
	}

	return log.WithFields(log.Fields{
		"activeKey":  activeKeyID,
		"pendingKey": pendingKeyID,
	})
}

func (ag *Autogenerated) publishAndRotate(rotateInterval time.Duration, publicationResult *keyserver.PublishResult) {
	defer close(ag.doneCh)

	// Create a channel that will tell us when we should rotate the key,
	// or never if `rotateInterval` is non-positive.
	timeToPublish := make(<-chan time.Time)
	if rotateInterval > 0 {
		ticker := time.NewTicker(rotateInterval)
		defer ticker.Stop()
		timeToPublish = ticker.C
	} else {
		log.Info("Key rotation is disabled")
	}

	for {
		select {
		case <-ag.stopCh:
			ag.getLogger().Info("Shutting down key publisher")
			publicationResult.Cancel()
			return
		case <-timeToPublish:
			// Start the publication process.
			ag.getLogger().Debug("Generating new key")
			publicationResult.Cancel()
			publicationResult = ag.attemptPublish(ag.active, rotateInterval)

		case publishError := <-publicationResult.Result():
			if publishError != nil {
				ag.getLogger().WithError(publishError).Fatal("Error publishing key")
			} else {
				// Publication was successful, swap the pending key to active.
				ag.keyLock.Lock()
				toSave := ag.pending
				ag.active = ag.pending
				ag.pending = nil
				ag.keyLock.Unlock()
				ag.getLogger().Debug("Successfully published key")

				// Asynchronously save the key to disk, best effort.
				go savePrivateKey(toSave, ag.keyPath)

				// We want to disable the publication error case for now.
				publicationResult = keyserver.NewPublishResult()
			}
		}
	}
}

func (ag *Autogenerated) revokeKey(toRevoke *key.PrivateKey) error {
	err := ag.manager.DeletePublicKey(toRevoke)
	if err != nil {
		log.Errorf("Unable to revoke pending key: ", err)
		return err
	}
	log.Debugf("Successfully revoked pending key")
	return nil
}