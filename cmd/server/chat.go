package main

import (
	"log"
	"sort"
	"strings"

	"the-fulfillment/backend/internal/chathub"
	"the-fulfillment/backend/internal/config"
	"the-fulfillment/backend/internal/repositories"
	"the-fulfillment/backend/internal/services"
)

// newChatService dựng chat nội bộ: hub WebSocket trong RAM (một instance API).
func newChatService(cfg *config.Config, repo *repositories.Repositories) *services.ChatService {
	roles := services.ParseChatRoles(cfg.ChatRoles)
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, string(r))
	}
	sort.Strings(names)
	log.Printf("chat: mở cho vai trò [%s], seller=%v", strings.Join(names, ","), cfg.ChatAllowSeller)
	return services.NewChatService(repo, chathub.New(), services.ChatOptions{
		Roles:       roles,
		AllowSeller: cfg.ChatAllowSeller,
	})
}
