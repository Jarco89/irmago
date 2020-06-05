package keysharetask

import (
	"bytes"
	"database/sql"
	"time"

	_ "github.com/jackc/pgx/stdlib"
	"github.com/privacybydesign/irmago/server"
)

type TaskHandler struct {
	conf *Configuration
	db   *sql.DB
}

func New(conf *Configuration) (*TaskHandler, error) {
	err := processConfiguration(conf)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", conf.DbConnstring)
	if err != nil {
		return nil, err
	}

	return &TaskHandler{
		db:   db,
		conf: conf,
	}, nil
}

func (t *TaskHandler) CleanupEmails() {
	_, err := t.db.Exec("DELETE FROM irma.email_addresses WHERE delete_on < $1", time.Now().Unix())
	if err != nil {
		t.conf.Logger.WithField("error", err).Error("Could not remove email addresses marked for deletion")
	}
}

func (t *TaskHandler) CleanupTokens() {
	_, err := t.db.Exec("DELETE FROM irma.email_login_tokens WHERE expiry < $1", time.Now().Unix())
	if err != nil {
		t.conf.Logger.WithField("error", err).Error("Could not remove email login tokens that have expired")
		return
	}
	_, err = t.db.Exec("DELETE FROM irma.email_verification_tokens WHERE expiry < $1", time.Now().Unix())
	if err != nil {
		t.conf.Logger.WithField("error", err).Error("Could not remove email verification tokens that have expired")
	}
}

func (t *TaskHandler) CleanupAccounts() {
	_, err := t.db.Exec("DELETE FROM irma.users WHERE delete_on < $1 AND (coredata IS NULL OR lastseen < delete_on - $2)",
		time.Now().Unix(),
		t.conf.DeleteDelay*24*60*60)
	if err != nil {
		t.conf.Logger.WithField("error", err).Error("Could not remove accounts scheduled for deletion")
	}
}

func (t *TaskHandler) ExpireAccounts() {
	// Disable this task when email server is not given
	if t.conf.EmailServer == "" {
		t.conf.Logger.Warning("Expiring accounts is disabled, as no email server is configured")
		return
	}

	res, err := t.db.Query(`SELECT id, username, language 
							FROM irma.users 
							WHERE lastseen < $1 
								AND (
										SELECT count(*) 
										FROM irma.email_addresses 
										WHERE irma.users.id = irma.email_addresses.user_id
									) > 0 
							LIMIT 10`,
		time.Now().Add(time.Duration(-24*t.conf.ExpiryDelay)*time.Hour).Unix())
	if err != nil {
		t.conf.Logger.WithField("error", err).Error("Could not query for accounts that have expired")
		return
	}
	defer res.Close()
	for res.Next() {
		var id int64
		var username string
		var lang string
		err = res.Scan(&id, &username, &lang)
		if err != nil {
			t.conf.Logger.WithField("error", err).Error("Could not retrieve expired account information")
			return
		}

		// Prepare email body
		template, ok := t.conf.DeleteExpiredAccountTemplate[lang]
		if !ok {
			template = t.conf.DeleteExpiredAccountTemplate[t.conf.DefaultLanguage]
		}
		subject, ok := t.conf.DeleteExpiredAccountSubject[lang]
		if !ok {
			subject = t.conf.DeleteExpiredAccountSubject[t.conf.DefaultLanguage]
		}
		var emsg bytes.Buffer

		err = template.Execute(&emsg, map[string]string{"username": username})
		if err != nil {
			t.conf.Logger.WithField("error", err).Error("Could not render email")
			return
		}

		// Fetch user's email addresses
		emailres, err := t.db.Query("SELECT emailAddress FROM irma.email_addresses WHERE user_id = $1", id)
		if err != nil {
			t.conf.Logger.WithField("error", err).Error("Could not retrieve user's email addresses")
			return
		}
		for emailres.Next() {
			var email string
			err = emailres.Scan(&email)
			if err != nil {
				t.conf.Logger.WithField("error", err).Error("Could not retrieve email address")
				return
			}

			server.SendHTMLMail(
				t.conf.EmailServer,
				t.conf.EmailAuth,
				t.conf.EmailFrom,
				email,
				subject,
				emsg.Bytes())
		}

		del, err := t.db.Exec("UPDATE irma.users SET delete_on = $2 WHERE id = $1", id,
			time.Now().Add(time.Duration(24*t.conf.DeleteDelay)*time.Hour).Unix())
		if err != nil {
			t.conf.Logger.WithField("error", err).WithField("id", id).Error("Could not mark user account for deletion")
			return
		}
		aff, err := del.RowsAffected()
		if err != nil {
			t.conf.Logger.WithField("error", err).WithField("id", id).Error("Could not mark user account for deletion")
			return
		}
		if aff != 1 {
			t.conf.Logger.WithField("error", err).WithField("id", id).Error("Could not mark user account for deletion")
			return
		}
	}
	err = res.Err()
	if err != nil {
		t.conf.Logger.WithField("error", err).Error("Error during iteration over accounts to be deleted")
	}
}
