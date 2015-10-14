// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package models

import (
	"bufio"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Unknwon/com"
	"github.com/go-xorm/xorm"

	"github.com/smallnewer/gogs/modules/log"
	"github.com/smallnewer/gogs/modules/process"
	"github.com/smallnewer/gogs/modules/setting"
)

const (
	// "### autogenerated by gitgos, DO NOT EDIT\n"
	_TPL_PUBLICK_KEY = `command="%s serv key-%d --config='%s'",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty %s` + "\n"
)

var (
	ErrKeyUnableVerify = errors.New("Unable to verify public key")
)

var sshOpLocker = sync.Mutex{}

var (
	SSHPath string // SSH directory.
	appPath string // Execution(binary) path.
)

// exePath returns the executable path.
func exePath() (string, error) {
	file, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(file)
}

// homeDir returns the home directory of current user.
func homeDir() string {
	home, err := com.HomeDir()
	if err != nil {
		log.Fatal(4, "Fail to get home directory: %v", err)
	}
	return home
}

func init() {
	var err error

	if appPath, err = exePath(); err != nil {
		log.Fatal(4, "fail to get app path: %v\n", err)
	}
	appPath = strings.Replace(appPath, "\\", "/", -1)

	// Determine and create .ssh path.
	SSHPath = filepath.Join(homeDir(), ".ssh")
	if err = os.MkdirAll(SSHPath, 0700); err != nil {
		log.Fatal(4, "fail to create '%s': %v", SSHPath, err)
	}
}

type KeyType int

const (
	KEY_TYPE_USER = iota + 1
	KEY_TYPE_DEPLOY
)

// PublicKey represents a SSH or deploy key.
type PublicKey struct {
	ID                int64      `xorm:"pk autoincr"`
	OwnerID           int64      `xorm:"INDEX NOT NULL"`
	Name              string     `xorm:"NOT NULL"`
	Fingerprint       string     `xorm:"NOT NULL"`
	Content           string     `xorm:"TEXT NOT NULL"`
	Mode              AccessMode `xorm:"NOT NULL DEFAULT 2"`
	Type              KeyType    `xorm:"NOT NULL DEFAULT 1"`
	Created           time.Time  `xorm:"CREATED"`
	Updated           time.Time  // Note: Updated must below Created for AfterSet.
	HasRecentActivity bool       `xorm:"-"`
	HasUsed           bool       `xorm:"-"`
}

func (k *PublicKey) AfterSet(colName string, _ xorm.Cell) {
	switch colName {
	case "created":
		k.HasUsed = k.Updated.After(k.Created)
		k.HasRecentActivity = k.Updated.Add(7 * 24 * time.Hour).After(time.Now())
	}
}

// OmitEmail returns content of public key but without e-mail address.
func (k *PublicKey) OmitEmail() string {
	return strings.Join(strings.Split(k.Content, " ")[:2], " ")
}

// GetAuthorizedString generates and returns formatted public key string for authorized_keys file.
func (key *PublicKey) GetAuthorizedString() string {
	return fmt.Sprintf(_TPL_PUBLICK_KEY, appPath, key.ID, setting.CustomConf, key.Content)
}

var minimumKeySizes = map[string]int{
	"(ED25519)": 256,
	"(ECDSA)":   256,
	"(NTRU)":    1087,
	"(MCE)":     1702,
	"(McE)":     1702,
	"(RSA)":     1024,
	"(DSA)":     1024,
}

func extractTypeFromBase64Key(key string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(b) < 4 {
		return "", errors.New("Invalid key format")
	}

	keyLength := int(binary.BigEndian.Uint32(b))

	if len(b) < 4+keyLength {
		return "", errors.New("Invalid key format")
	}

	return string(b[4 : 4+keyLength]), nil
}

// parseKeyString parses any key string in openssh or ssh2 format to clean openssh string (rfc4253)
func parseKeyString(content string) (string, error) {
	// Transform all legal line endings to a single "\n"
	s := strings.Replace(strings.Replace(strings.TrimSpace(content), "\r\n", "\n", -1), "\r", "\n", -1)

	lines := strings.Split(s, "\n")

	var keyType, keyContent, keyComment string

	if len(lines) == 1 {
		// Parse openssh format
		parts := strings.SplitN(lines[0], " ", 3)
		switch len(parts) {
		case 0:
			return "", errors.New("Empty key")
		case 1:
			keyContent = parts[0]
		case 2:
			keyType = parts[0]
			keyContent = parts[1]
		default:
			keyType = parts[0]
			keyContent = parts[1]
			keyComment = parts[2]
		}

		// If keyType is not given, extract it from content. If given, validate it
		if len(keyType) == 0 {
			if t, err := extractTypeFromBase64Key(keyContent); err == nil {
				keyType = t
			} else {
				return "", err
			}
		} else {
			if t, err := extractTypeFromBase64Key(keyContent); err != nil || keyType != t {
				return "", err
			}
		}
	} else {
		// Parse SSH2 file format.
		continuationLine := false

		for _, line := range lines {
			// Skip lines that:
			// 1) are a continuation of the previous line,
			// 2) contain ":" as that are comment lines
			// 3) contain "-" as that are begin and end tags
			if continuationLine || strings.ContainsAny(line, ":-") {
				continuationLine = strings.HasSuffix(line, "\\")
			} else {
				keyContent = keyContent + line
			}
		}

		if t, err := extractTypeFromBase64Key(keyContent); err == nil {
			keyType = t
		} else {
			return "", err
		}
	}
	return keyType + " " + keyContent + " " + keyComment, nil
}

// CheckPublicKeyString checks if the given public key string is recognized by SSH.
func CheckPublicKeyString(content string) (_ string, err error) {
	content, err = parseKeyString(content)
	if err != nil {
		return "", err
	}

	content = strings.TrimRight(content, "\n\r")
	if strings.ContainsAny(content, "\n\r") {
		return "", errors.New("only a single line with a single key please")
	}

	// write the key to a file…
	tmpFile, err := ioutil.TempFile(os.TempDir(), "keytest")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	tmpFile.WriteString(content)
	tmpFile.Close()

	// Check if ssh-keygen recognizes its contents.
	stdout, stderr, err := process.Exec("CheckPublicKeyString", "ssh-keygen", "-l", "-f", tmpPath)
	if err != nil {
		return "", errors.New("ssh-keygen -l -f: " + stderr)
	} else if len(stdout) < 2 {
		return "", errors.New("ssh-keygen returned not enough output to evaluate the key: " + stdout)
	}

	// The ssh-keygen in Windows does not print key type, so no need go further.
	if setting.IsWindows {
		return content, nil
	}

	sshKeygenOutput := strings.Split(stdout, " ")
	if len(sshKeygenOutput) < 4 {
		return content, ErrKeyUnableVerify
	}

	// Check if key type and key size match.
	if !setting.Service.DisableMinimumKeySizeCheck {
		keySize := com.StrTo(sshKeygenOutput[0]).MustInt()
		if keySize == 0 {
			return "", errors.New("cannot get key size of the given key")
		}
		keyType := strings.TrimSpace(sshKeygenOutput[len(sshKeygenOutput)-1])
		if minimumKeySize := minimumKeySizes[keyType]; minimumKeySize == 0 {
			return "", errors.New("sorry, unrecognized public key type")
		} else if keySize < minimumKeySize {
			return "", fmt.Errorf("the minimum accepted size of a public key %s is %d", keyType, minimumKeySize)
		}
	}

	return content, nil
}

// saveAuthorizedKeyFile writes SSH key content to authorized_keys file.
func saveAuthorizedKeyFile(keys ...*PublicKey) error {
	sshOpLocker.Lock()
	defer sshOpLocker.Unlock()

	fpath := filepath.Join(SSHPath, "authorized_keys")
	f, err := os.OpenFile(fpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	// FIXME: following command does not support in Windows.
	if !setting.IsWindows {
		// .ssh directory should have mode 700, and authorized_keys file should have mode 600.
		if fi.Mode().Perm() > 0600 {
			log.Error(4, "authorized_keys file has unusual permission flags: %s - setting to -rw-------", fi.Mode().Perm().String())
			if err = f.Chmod(0600); err != nil {
				return err
			}
		}
	}

	for _, key := range keys {
		if _, err = f.WriteString(key.GetAuthorizedString()); err != nil {
			return err
		}
	}
	return nil
}

// checkKeyContent onlys checks if key content has been used as public key,
// it is OK to use same key as deploy key for multiple repositories/users.
func checkKeyContent(content string) error {
	has, err := x.Get(&PublicKey{
		Content: content,
		Type:    KEY_TYPE_USER,
	})
	if err != nil {
		return err
	} else if has {
		return ErrKeyAlreadyExist{0, content}
	}
	return nil
}

func addKey(e Engine, key *PublicKey) (err error) {
	// Calculate fingerprint.
	tmpPath := strings.Replace(path.Join(os.TempDir(), fmt.Sprintf("%d", time.Now().Nanosecond()),
		"id_rsa.pub"), "\\", "/", -1)
	os.MkdirAll(path.Dir(tmpPath), os.ModePerm)
	if err = ioutil.WriteFile(tmpPath, []byte(key.Content), 0644); err != nil {
		return err
	}
	stdout, stderr, err := process.Exec("AddPublicKey", "ssh-keygen", "-l", "-f", tmpPath)
	if err != nil {
		return errors.New("ssh-keygen -l -f: " + stderr)
	} else if len(stdout) < 2 {
		return errors.New("not enough output for calculating fingerprint: " + stdout)
	}
	key.Fingerprint = strings.Split(stdout, " ")[1]

	// Save SSH key.
	if _, err = e.Insert(key); err != nil {
		return err
	}
	return saveAuthorizedKeyFile(key)
}

// AddPublicKey adds new public key to database and authorized_keys file.
func AddPublicKey(ownerID int64, name, content string) (err error) {
	if err = checkKeyContent(content); err != nil {
		return err
	}

	// Key name of same user cannot be duplicated.
	has, err := x.Where("owner_id=? AND name=?", ownerID, name).Get(new(PublicKey))
	if err != nil {
		return err
	} else if has {
		return ErrKeyNameAlreadyUsed{ownerID, name}
	}

	sess := x.NewSession()
	defer sessionRelease(sess)
	if err = sess.Begin(); err != nil {
		return err
	}

	key := &PublicKey{
		OwnerID: ownerID,
		Name:    name,
		Content: content,
		Mode:    ACCESS_MODE_WRITE,
		Type:    KEY_TYPE_USER,
	}
	if err = addKey(sess, key); err != nil {
		return fmt.Errorf("addKey: %v", err)
	}

	return sess.Commit()
}

// GetPublicKeyByID returns public key by given ID.
func GetPublicKeyByID(keyID int64) (*PublicKey, error) {
	key := new(PublicKey)
	has, err := x.Id(keyID).Get(key)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrKeyNotExist{keyID}
	}
	return key, nil
}

// ListPublicKeys returns a list of public keys belongs to given user.
func ListPublicKeys(uid int64) ([]*PublicKey, error) {
	keys := make([]*PublicKey, 0, 5)
	return keys, x.Where("owner_id=?", uid).Find(&keys)
}

// rewriteAuthorizedKeys finds and deletes corresponding line in authorized_keys file.
func rewriteAuthorizedKeys(key *PublicKey, p, tmpP string) error {
	fr, err := os.Open(p)
	if err != nil {
		return err
	}
	defer fr.Close()

	fw, err := os.OpenFile(tmpP, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer fw.Close()

	isFound := false
	keyword := fmt.Sprintf("key-%d", key.ID)
	buf := bufio.NewReader(fr)
	for {
		line, errRead := buf.ReadString('\n')
		line = strings.TrimSpace(line)

		if errRead != nil {
			if errRead != io.EOF {
				return errRead
			}

			// Reached end of file, if nothing to read then break,
			// otherwise handle the last line.
			if len(line) == 0 {
				break
			}
		}

		// Found the line and copy rest of file.
		if !isFound && strings.Contains(line, keyword) && strings.Contains(line, key.Content) {
			isFound = true
			continue
		}
		// Still finding the line, copy the line that currently read.
		if _, err = fw.WriteString(line + "\n"); err != nil {
			return err
		}

		if errRead == io.EOF {
			break
		}
	}
	return nil
}

// UpdatePublicKey updates given public key.
func UpdatePublicKey(key *PublicKey) error {
	_, err := x.Id(key.ID).AllCols().Update(key)
	return err
}

func deletePublicKey(e *xorm.Session, keyID int64) error {
	sshOpLocker.Lock()
	defer sshOpLocker.Unlock()

	key := &PublicKey{ID: keyID}
	has, err := e.Get(key)
	if err != nil {
		return err
	} else if !has {
		return nil
	}

	if _, err = e.Id(key.ID).Delete(new(PublicKey)); err != nil {
		return err
	}

	fpath := filepath.Join(SSHPath, "authorized_keys")
	tmpPath := filepath.Join(SSHPath, "authorized_keys.tmp")
	if err = rewriteAuthorizedKeys(key, fpath, tmpPath); err != nil {
		return err
	} else if err = os.Remove(fpath); err != nil {
		return err
	}
	return os.Rename(tmpPath, fpath)
}

// DeletePublicKey deletes SSH key information both in database and authorized_keys file.
func DeletePublicKey(id int64) (err error) {
	has, err := x.Id(id).Get(new(PublicKey))
	if err != nil {
		return err
	} else if !has {
		return nil
	}

	sess := x.NewSession()
	defer sessionRelease(sess)
	if err = sess.Begin(); err != nil {
		return err
	}

	if err = deletePublicKey(sess, id); err != nil {
		return err
	}

	return sess.Commit()
}

// RewriteAllPublicKeys removes any authorized key and rewrite all keys from database again.
func RewriteAllPublicKeys() error {
	sshOpLocker.Lock()
	defer sshOpLocker.Unlock()

	tmpPath := filepath.Join(SSHPath, "authorized_keys.tmp")
	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	err = x.Iterate(new(PublicKey), func(idx int, bean interface{}) (err error) {
		_, err = f.WriteString((bean.(*PublicKey)).GetAuthorizedString())
		return err
	})
	f.Close()
	if err != nil {
		return err
	}

	fpath := filepath.Join(SSHPath, "authorized_keys")
	if com.IsExist(fpath) {
		if err = os.Remove(fpath); err != nil {
			return err
		}
	}
	if err = os.Rename(tmpPath, fpath); err != nil {
		return err
	}

	return nil
}

// ________                .__                 ____  __.
// \______ \   ____ ______ |  |   ____ ___.__.|    |/ _|____ ___.__.
//  |    |  \_/ __ \\____ \|  |  /  _ <   |  ||      <_/ __ <   |  |
//  |    `   \  ___/|  |_> >  |_(  <_> )___  ||    |  \  ___/\___  |
// /_______  /\___  >   __/|____/\____// ____||____|__ \___  > ____|
//         \/     \/|__|               \/             \/   \/\/

// DeployKey represents deploy key information and its relation with repository.
type DeployKey struct {
	ID                int64 `xorm:"pk autoincr"`
	KeyID             int64 `xorm:"UNIQUE(s) INDEX"`
	RepoID            int64 `xorm:"UNIQUE(s) INDEX"`
	Name              string
	Fingerprint       string
	Created           time.Time `xorm:"CREATED"`
	Updated           time.Time // Note: Updated must below Created for AfterSet.
	HasRecentActivity bool      `xorm:"-"`
	HasUsed           bool      `xorm:"-"`
}

func (k *DeployKey) AfterSet(colName string, _ xorm.Cell) {
	switch colName {
	case "created":
		k.HasUsed = k.Updated.After(k.Created)
		k.HasRecentActivity = k.Updated.Add(7 * 24 * time.Hour).After(time.Now())
	}
}

func checkDeployKey(e Engine, keyID, repoID int64, name string) error {
	// Note: We want error detail, not just true or false here.
	has, err := e.Where("key_id=? AND repo_id=?", keyID, repoID).Get(new(DeployKey))
	if err != nil {
		return err
	} else if has {
		return ErrDeployKeyAlreadyExist{keyID, repoID}
	}

	has, err = e.Where("repo_id=? AND name=?", repoID, name).Get(new(DeployKey))
	if err != nil {
		return err
	} else if has {
		return ErrDeployKeyNameAlreadyUsed{repoID, name}
	}

	return nil
}

// addDeployKey adds new key-repo relation.
func addDeployKey(e *xorm.Session, keyID, repoID int64, name, fingerprint string) (err error) {
	if err = checkDeployKey(e, keyID, repoID, name); err != nil {
		return err
	}

	_, err = e.Insert(&DeployKey{
		KeyID:       keyID,
		RepoID:      repoID,
		Name:        name,
		Fingerprint: fingerprint,
	})
	return err
}

// HasDeployKey returns true if public key is a deploy key of given repository.
func HasDeployKey(keyID, repoID int64) bool {
	has, _ := x.Where("key_id=? AND repo_id=?", keyID, repoID).Get(new(DeployKey))
	return has
}

// AddDeployKey add new deploy key to database and authorized_keys file.
func AddDeployKey(repoID int64, name, content string) (err error) {
	if err = checkKeyContent(content); err != nil {
		return err
	}

	key := &PublicKey{
		Content: content,
		Mode:    ACCESS_MODE_READ,
		Type:    KEY_TYPE_DEPLOY,
	}
	has, err := x.Get(key)
	if err != nil {
		return err
	}

	sess := x.NewSession()
	defer sessionRelease(sess)
	if err = sess.Begin(); err != nil {
		return err
	}

	// First time use this deploy key.
	if !has {
		if err = addKey(sess, key); err != nil {
			return nil
		}
	}

	if err = addDeployKey(sess, key.ID, repoID, name, key.Fingerprint); err != nil {
		return err
	}

	return sess.Commit()
}

// GetDeployKeyByRepo returns deploy key by given public key ID and repository ID.
func GetDeployKeyByRepo(keyID, repoID int64) (*DeployKey, error) {
	key := &DeployKey{
		KeyID:  keyID,
		RepoID: repoID,
	}
	_, err := x.Get(key)
	return key, err
}

// UpdateDeployKey updates deploy key information.
func UpdateDeployKey(key *DeployKey) error {
	_, err := x.Id(key.ID).AllCols().Update(key)
	return err
}

// DeleteDeployKey deletes deploy key from its repository authorized_keys file if needed.
func DeleteDeployKey(id int64) error {
	key := &DeployKey{ID: id}
	has, err := x.Id(key.ID).Get(key)
	if err != nil {
		return err
	} else if !has {
		return nil
	}

	sess := x.NewSession()
	defer sessionRelease(sess)
	if err = sess.Begin(); err != nil {
		return err
	}

	if _, err = sess.Id(key.ID).Delete(new(DeployKey)); err != nil {
		return fmt.Errorf("delete deploy key[%d]: %v", key.ID, err)
	}

	// Check if this is the last reference to same key content.
	has, err = sess.Where("key_id=?", key.KeyID).Get(new(DeployKey))
	if err != nil {
		return err
	} else if !has {
		if err = deletePublicKey(sess, key.KeyID); err != nil {
			return err
		}
	}

	return sess.Commit()
}

// ListDeployKeys returns all deploy keys by given repository ID.
func ListDeployKeys(repoID int64) ([]*DeployKey, error) {
	keys := make([]*DeployKey, 0, 5)
	return keys, x.Where("repo_id=?", repoID).Find(&keys)
}
