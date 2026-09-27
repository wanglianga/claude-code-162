package service

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"recycle/internal/models"
)

// Login 校验账号密码。
func (s *Service) Login(ctx context.Context, phone, password string) (*models.Account, error) {
	var a models.Account
	err := s.db.QueryRow(ctx,
		`SELECT id, role, name, phone, password_hash, created_at FROM accounts WHERE phone=$1`,
		phone).Scan(&a.ID, &a.Role, &a.Name, &a.Phone, &a.PasswordHash, &a.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	return &a, nil
}

// AccountByID 查询账号。
func (s *Service) AccountByID(ctx context.Context, id int64) (*models.Account, error) {
	var a models.Account
	err := s.db.QueryRow(ctx,
		`SELECT id, role, name, phone, '', created_at FROM accounts WHERE id=$1`,
		id).Scan(&a.ID, &a.Role, &a.Name, &a.Phone, &a.PasswordHash, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Service) createAccount(ctx context.Context, t pgx.Tx, role, name, phone, password string) (int64, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, err
	}
	var id int64
	err = t.QueryRow(ctx,
		`INSERT INTO accounts(role,name,phone,password_hash) VALUES($1,$2,$3,$4) RETURNING id`,
		role, name, phone, string(hash)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create account %s: %w", phone, err)
	}
	return id, nil
}
