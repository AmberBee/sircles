package auth

import (
	"context"

	"github.com/sorintlab/sircles/config"
	"github.com/sorintlab/sircles/db"
	"github.com/sorintlab/sircles/models"
	"github.com/sorintlab/sircles/readdb"
)

type localAuthenticator struct {
	config *config.LocalAuthConfig
	db     *db.DB
}

func NewLocalAuthenticator(config *config.LocalAuthConfig, db *db.DB) *localAuthenticator {
	return &localAuthenticator{config: config, db: db}
}

func (l *localAuthenticator) Login(ctx context.Context, loginName, password string) (string, error) {
	tx, err := l.db.NewTx()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	readDB, err := readdb.NewDBService(tx)
	if err != nil {
		return "", err
	}

	var member *models.Member
	if l.config.UseEmail {
		member, err = readDB.AuthenticateEmailPassword(loginName, password)
	} else {
		member, err = readDB.AuthenticateUserNamePassword(loginName, password)
	}
	if err != nil {
		return "", err
	}

	matchUID, err := readDB.MemberMatchUID(ctx, member.ID)
	if err != nil {
		return "", err
	}

	// if the member has not a matchUID we return the UserName
	if matchUID == "" {
		return member.UserName, nil
	}
	return matchUID, nil
}
