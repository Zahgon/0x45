package middleware

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/watzon/0x45/internal/config"
	"github.com/watzon/0x45/internal/httperr"
	"github.com/watzon/0x45/internal/ratelimit"
)

type RateLimiter struct {
	logger  *zap.Logger
	config  *config.Config
	limiter *ratelimit.RateLimiter
}

func NewRateLimiter(logger *zap.Logger, config *config.Config) *RateLimiter {
	// Create rate limiter config from server config
	limiterConfig := ratelimit.Config{
		Global: struct {
			Enabled bool
			Rate    float64
			Burst   int
		}{
			Enabled: config.Server.RateLimit.Global.Enabled,
			Rate:    config.Server.RateLimit.Global.Rate,
			Burst:   config.Server.RateLimit.Global.Burst,
		},
		PerIP: struct {
			Enabled bool
			Rate    float64
			Burst   int
		}{
			Enabled: config.Server.RateLimit.PerIP.Enabled,
			Rate:    config.Server.RateLimit.PerIP.Rate,
			Burst:   config.Server.RateLimit.PerIP.Burst,
		},
		UseRedis: config.Redis.Enabled,
	}

	if config.Redis.Enabled {
		redisClient := redis.NewClient(&redis.Options{
			Addr:     config.Redis.Address,
			Password: config.Redis.Password,
			DB:       config.Redis.DB,
		})

		// Test Redis connection
		if _, err := redisClient.Ping(context.Background()).Result(); err != nil {
			logger.Error("failed to connect to Redis", zap.Error(err))
		}

		limiterConfig.Redis = redisClient
	}

	return &RateLimiter{
		logger:  logger,
		config:  config,
		limiter: ratelimit.New(limiterConfig),
	}
}

// RateLimit returns a middleware that limits requests
func (m *RateLimiter) RateLimit() gin.HandlerFunc {
	return httperr.Wrap(func(c *gin.Context) error {
		// Skip rate limiting on non-API routes
		if !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Next()
			return nil
		}

		// Skip rate limiting if request has a valid API key
		if _, ok := c.Get("apiKey"); ok {
			c.Next()
			return nil
		}

		// Use the existing rate limiter implementation
		if err := m.limiter.Check(c.ClientIP()); err != nil {
			m.logger.Warn("rate limit exceeded",
				zap.String("ip", c.ClientIP()),
				zap.Error(err),
			)
			return err
		}

		c.Next()
		return nil
	})
}
