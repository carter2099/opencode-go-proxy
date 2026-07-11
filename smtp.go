package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// smtpConfig holds the bot SMTP credentials read from the shared .smtp_config file.
type smtpConfig struct {
	Address  string
	Port     string
	Username string
	Password string
}

func loadSMTPConfig(path string) (smtpConfig, error) {
	var c smtpConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read smtp config %s: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		eq := strings.Index(line, "=")
		if eq == -1 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		switch k {
		case "SMTP_ADDRESS":
			c.Address = v
		case "SMTP_PORT":
			c.Port = v
		case "SMTP_USERNAME":
			c.Username = v
		case "SMTP_PASSWORD":
			c.Password = v
		}
	}
	if c.Address == "" || c.Port == "" || c.Username == "" || c.Password == "" {
		return c, fmt.Errorf("smtp config %s: missing one of SMTP_ADDRESS/SMTP_PORT/SMTP_USERNAME/SMTP_PASSWORD", path)
	}
	return c, nil
}

// sendAlertEmail emails a cookie-stale alert. Returns nil if the alert should
// be suppressed (re-alert window not yet elapsed) — "shouldSend" is decided by
// the caller before constructing the message; this function only sends.
func sendAlertEmail(cfg Config, acct *account, lastGood time.Time) error {
	sc, err := loadSMTPConfig(cfg.SMTPConfigPath)
	if err != nil {
		return err
	}
	from := sc.Username
	to := cfg.AlertEmail
	subject := fmt.Sprintf("[opencode-go-proxy] Cookie stale: %s", acct.cfg.Name)
	body := fmt.Sprintf("The auth cookie for account %q has gone stale.\n\n"+
		"Workspace: %s\n"+
		"Last good scrape: %s\n"+
		"Last scrape error: %s\n\n"+
		"Fix: refresh that account's auth cookie on opencode.ai, update "+
		"the auth_cookie field in ~/.config/opencode-go-proxy/config.json, "+
		"and restart the service:\n"+
		"  systemctl --user restart opencode-go-proxy\n",
		acct.cfg.Name, acct.cfg.WorkspaceID,
		humanTime(lastGood), safeErr(acct.lastError))

	msg := buildRFC822(from, to, subject, body)

	if err := smtpSend(sc, from, to, []byte(msg)); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}

func humanTime(t time.Time) string {
	if t.IsZero() {
		return "(never successfully scraped)"
	}
	return t.UTC().Format(time.RFC3339)
}

func safeErr(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// buildRFC822 assembles a minimal plain-text email.
func buildRFC822(from, to, subject, body string) string {
	headers := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Message-Id: <opencode-go-proxy-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@carter2099.com>",
	}
	return strings.Join(headers, "\r\n") + "\r\n\r\n" + body
}

// smtpSend opens a STARTTLS connection and authenticates with PLAIN.
func smtpSend(sc smtpConfig, from, to string, msg []byte) error {
	addr := net.JoinHostPort(sc.Address, sc.Port)
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, sc.Address)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Quit()
	if err := c.StartTLS(&tls.Config{ServerName: sc.Address}); err != nil {
		return fmt.Errorf("starttls: %w", err)
	}
	auth := smtp.PlainAuth("", sc.Username, sc.Password, sc.Address)
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}