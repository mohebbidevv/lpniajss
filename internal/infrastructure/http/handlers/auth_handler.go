package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"golaunch/internal/application"
	middleware "golaunch/internal/infrastructure/http/middlewares"
)

type AuthHandler struct {
	RegisterUseCase *application.RegisterUserUseCase
	LoginUseCase    *application.LoginUserUseCase
	LogoutUseCase   *application.LogoutUserUseCase
	SecureCookies   bool

	// Limiter throttles failed sign-ins. Nil disables throttling, which is
	// only appropriate in tests.
	Limiter *middleware.LoginLimiter

	RequestResetUseCase  *application.RequestPasswordResetUseCase
	ResetPasswordUseCase *application.ResetPasswordUseCase
	RequestVerifyUseCase *application.RequestEmailVerificationUseCase
	VerifyEmailUseCase   *application.VerifyEmailUseCase
}

func NewAuthHandler(
	register *application.RegisterUserUseCase,
	login *application.LoginUserUseCase,
	logout *application.LogoutUserUseCase,
	secureCookies bool,
	limiter *middleware.LoginLimiter,
) *AuthHandler {
	return &AuthHandler{
		RegisterUseCase: register,
		LoginUseCase:    login,
		LogoutUseCase:   logout,
		SecureCookies:   secureCookies,
		Limiter:         limiter,
	}
}

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *AuthHandler) Register(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	user, err := h.RegisterUseCase.Execute(r.Context(), req.Email, req.Password)
	if err != nil {
		status := http.StatusBadRequest
		if isConflict(err) {
			status = http.StatusConflict
		}
		respondError(w, err, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(user)
}

func (h *AuthHandler) Login(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Keyed on the normalized address so casing variants share one budget,
	// and checked here — before the use case runs — so a throttled attempt
	// never reaches bcrypt.
	ip := middleware.ClientIP(r)
	account := application.NormalizeEmail(req.Email)

	if h.Limiter != nil && !h.Limiter.Allow(ip, account) {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "too many failed sign-in attempts, try again later", http.StatusTooManyRequests)
		return
	}

	rawToken, user, err := h.LoginUseCase.Execute(r.Context(), req.Email, req.Password)
	if err != nil {
		if h.Limiter != nil {
			h.Limiter.RecordFailure(ip, account)
		}
		respondError(w, err, http.StatusUnauthorized)
		return
	}
	if h.Limiter != nil {
		h.Limiter.RecordSuccess(account)
	}

	h.setSessionCookie(w, rawToken)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(user)
}

func (h *AuthHandler) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	if cookie, err := r.Cookie(middleware.SessionCookieName); err == nil {
		_ = h.LogoutUseCase.Execute(r.Context(), cookie.Value)
	}

	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *AuthHandler) Me(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(user)
}

func (h *AuthHandler) setSessionCookie(w http.ResponseWriter, rawToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     middleware.SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(application.SessionTTL),
		// MaxAge alongside Expires: older browsers honour only one or the
		// other, and a session cookie that silently becomes a
		// browser-session cookie would log users out on every restart.
		MaxAge: int(application.SessionTTL.Seconds()),
	})
}

func (h *AuthHandler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     middleware.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

type emailOnlyRequest struct {
	Email string `json:"email"`
}

type tokenPasswordRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

type tokenRequest struct {
	Token string `json:"token"`
}

// RequestPasswordReset always answers 202, whether or not the address has an
// account. Any status that varied by existence would be an enumeration
// oracle, which is the whole thing the identical login error avoids.
func (h *AuthHandler) RequestPasswordReset(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req emailOnlyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Reuse the login limiter, keyed the same way: without it this endpoint
	// is an unauthenticated way to make the platform send mail to any
	// address, as fast as the attacker can post.
	ip := middleware.ClientIP(r)
	account := application.NormalizeEmail(req.Email)
	if h.Limiter != nil && !h.Limiter.Allow(ip, account) {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "too many requests, try again later", http.StatusTooManyRequests)
		return
	}
	if h.Limiter != nil {
		h.Limiter.RecordFailure(ip, account)
	}

	if err := h.RequestResetUseCase.Execute(r.Context(), req.Email); err != nil {
		respondError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *AuthHandler) ResetPassword(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req tokenPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.ResetPasswordUseCase.Execute(r.Context(), req.Token, req.Password); err != nil {
		respondError(w, err, http.StatusBadRequest)
		return
	}

	// Every session was just revoked, including possibly this caller's.
	h.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// RequestEmailVerification reissues a link for the authenticated caller, so
// unlike password reset there is nothing to enumerate here.
func (h *AuthHandler) RequestEmailVerification(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	user, ok := middleware.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	if err := h.RequestVerifyUseCase.Execute(r.Context(), user.ID); err != nil {
		respondError(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *AuthHandler) VerifyEmail(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req tokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if err := h.VerifyEmailUseCase.Execute(r.Context(), req.Token); err != nil {
		respondError(w, err, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
