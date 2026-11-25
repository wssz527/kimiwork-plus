package kimi

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

const (
	conversationListLimit      = 100
	conversationRebindLimit    = 500
	conversationWriteTimeout   = 30 * time.Second
	conversationRecoverTimeout = 5 * time.Second
	conversationJournalName    = ".kwp-conversation-rebind.journal.json"
)

var (
	errConversationStopFailed     = errors.New("conversation_kimi_stop_failed")
	errConversationRollbackFailed = errors.New("conversation_rollback_failed")
)

//go:embed conversation_sqlite.js
var conversationSQLiteScript string

type WorkConversation struct {
	ConversationKey      string `json:"conversationKey"`
	Title                string `json:"title"`
	WorkspacePath        string `json:"workspacePath"`
	UpdatedAtMs          int64  `json:"updatedAtMs"`
	ModelAlias           string `json:"modelAlias"`
	MigrationTargetAlias string `json:"migrationTargetAlias,omitempty"`
	ThinkingLevel        string `json:"thinkingLevel"`
	IsBYOK               bool   `json:"isByok"`
	FirstUserText        string `json:"firstUserText,omitempty"`
}

type ConversationModel struct {
	Alias                         string   `json:"alias"`
	DisplayName                   string   `json:"displayName"`
	Protocol                      string   `json:"protocol,omitempty"`
	SourceKind                    string   `json:"sourceKind"`
	PreviousAlias                 string   `json:"previousAlias,omitempty"`
	MigrationCount                int      `json:"migrationCount,omitempty"`
	SupportEfforts                []string `json:"supportEfforts"`
	DefaultEffort                 string   `json:"defaultEffort"`
	DirectAnthropicHistoryWarning bool     `json:"directAnthropicHistoryWarning"`
}

type ConversationList struct {
	Conversations []WorkConversation  `json:"conversations"`
	Models        []ConversationModel `json:"models"`
	Application   RunningState        `json:"application"`
	KimiRunning   bool                `json:"kimiRunning"`
	Reason        string              `json:"reason,omitempty"`
}

type ConversationRebindRequest struct {
	ConversationKeys     []string `json:"conversationKeys,omitempty"`
	TargetModelAlias     string   `json:"targetModelAlias,omitempty"`
	ThinkingLevel        string   `json:"thinkingLevel,omitempty"`
	RestartKimi          bool     `json:"restartKimi"`
	MigratePreviousAlias bool     `json:"migratePreviousAlias"`
}

type ConversationRebindResult struct {
	ConversationKeys              []string `json:"conversationKeys"`
	TargetModelAlias              string   `json:"targetModelAlias"`
	ThinkingLevel                 string   `json:"thinkingLevel"`
	HasMore                       bool     `json:"hasMore,omitempty"`
	DirectAnthropicHistoryWarning bool     `json:"directAnthropicHistoryWarning"`
	KimiRestarted                 bool     `json:"kimiRestarted"`
	RestartReason                 string   `json:"restartReason,omitempty"`
}

type conversationMigration struct {
	TargetModelAlias string   `json:"targetModelAlias"`
	Aliases          []string `json:"aliases,omitempty"`
	GatewayModelID   string   `json:"gatewayModelId,omitempty"`
	ThinkingLevel    string   `json:"thinkingLevel,omitempty"`
	SupportEfforts   []string `json:"supportEfforts,omitempty"`
}

type conversationSQLiteRequest struct {
	Operation        string                  `json:"operation"`
	Database         string                  `json:"database"`
	AgentDirectory   string                  `json:"agentDirectory,omitempty"`
	Limit            int                     `json:"limit,omitempty"`
	ConversationKeys []string                `json:"conversationKeys,omitempty"`
	ModelAlias       string                  `json:"modelAlias,omitempty"`
	ThinkingLevel    string                  `json:"thinkingLevel,omitempty"`
	Migrations       []conversationMigration `json:"migrations,omitempty"`
	ForeignProviders []string                `json:"foreignProviders,omitempty"`
}

type conversationSQLiteResult struct {
	Conversations    []WorkConversation `json:"conversations"`
	ConversationKeys []string           `json:"conversationKeys"`
	HasMore          bool               `json:"hasMore"`
	HadOfficial      *bool              `json:"hadOfficial"`
	Recovered        string             `json:"recovered"`
	MigrationCounts  map[string]int     `json:"migrationCounts"`
	Error            string             `json:"error"`
}

type conversationStore struct {
	profile       string
	home          string
	install       Installation
	nodePath      string
	catalog       []Model
	mode          string
	directCatalog *DirectSpec
	stateDir      string
}

func (c *Controller) conversationStore() (conversationStore, error) {
	settings := c.Settings()
	if settings.profileMode() == ProfileModeExistingAuthorized {
		return conversationStore{}, errors.New("conversation_profile_unsupported")
	}
	home, err := c.WorkHome()
	return conversationStore{profile: settings.UserDataDir, home: home, install: settings.Installation,
		nodePath: settings.ConversationNodePath, catalog: settings.Models, mode: settings.integrationMode(),
		directCatalog: settings.ConversationDirectSpec, stateDir: settings.StateDir}, err
}

func (c *Controller) WorkConversations(ctx context.Context) ConversationList {
	settings := c.Settings()
	result := ConversationList{Conversations: []WorkConversation{}, Models: []ConversationModel{}}
	result.Application = conversationApplication(ctx, settings.Installation.Application)
	result.KimiRunning = result.Application == Running
	store, err := c.conversationStore()
	if err != nil {
		result.Reason = stableErrorCode(err, "conversation_database_unavailable")
		return result
	}
	if store.hasPendingJournal() {
		c.lifecycle.Lock()
		store, err = c.conversationStore()
		if err == nil {
			result.Application = conversationApplication(ctx, store.install.Application)
			result.KimiRunning = result.Application == Running
			if store.hasPendingJournal() {
				err = errConversationRollbackFailed
				if result.Application == NotRunning && !c.managedChildAlive() {
					err = store.recoverBindings(ctx)
				}
			}
		}
		c.lifecycle.Unlock()
		if err != nil {
			result.Reason = stableErrorCode(err, "conversation_database_unavailable")
			return result
		}
	}
	catalog := store.models()
	result.Models = catalog.models
	readCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	reply, err := store.run(readCtx, conversationSQLiteRequest{Operation: "list", Limit: conversationListLimit,
		Migrations: catalog.migrations, ForeignProviders: catalog.foreignProviders})
	if store.hasPendingJournal() {
		result.Reason = errConversationRollbackFailed.Error()
		return result
	}
	if err != nil {
		result.Reason = stableErrorCode(err, "conversation_database_unavailable")
		return result
	}
	for i := range result.Models {
		result.Models[i].MigrationCount = reply.MigrationCounts[result.Models[i].Alias]
	}
	for _, row := range reply.Conversations {
		if strings.TrimSpace(row.Title) == "" {
			row.Title = strings.Join(strings.Fields(row.FirstUserText), " ")
			runes := []rune(row.Title)
			if len(runes) > 80 {
				row.Title = string(runes[:80]) + "…"
			}
		}
		row.FirstUserText = ""
		row.IsBYOK = strings.HasPrefix(row.ModelAlias, "byok-")
		result.Conversations = append(result.Conversations, row)
	}
	return result
}

func conversationApplication(ctx context.Context, application string) RunningState {
	if application == "" {
		return RunningStateUnknown
	}
	check, cancel := context.WithTimeout(ctx, IdentityProbeTimeout)
	defer cancel()
	pids, known, _ := listOfficialProcesses(check, application)
	if !known || check.Err() != nil {
		return RunningStateUnknown
	}
	if len(pids) > 0 {
		return Running
	}
	return NotRunning
}

func (c *Controller) RebindWorkConversations(ctx context.Context, request ConversationRebindRequest) (ConversationRebindResult, error) {
	var result ConversationRebindResult
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if err := validateConversationRebind(request); err != nil {
		return result, err
	}
	settings := c.Settings()
	store, err := c.conversationStore()
	if err != nil {
		return result, errors.New(stableErrorCode(err, "conversation_database_unavailable"))
	}
	target, mutation, err := store.rebindMutation(request)
	if err != nil {
		return result, err
	}
	application := settings.Installation.Application
	initial := conversationApplication(ctx, application)
	switch initial {
	case RunningStateUnknown:
		return result, errors.New("conversation_kimi_state_unknown")
	case Running:
		if !request.RestartKimi {
			return result, errors.New("conversation_kimi_restart_required")
		}
		if err := stopOfficialProcesses(ctx, application); err != nil {
			return result, errConversationStopFailed
		}
		c.awaitManagedChildExit(ctx)
		c.reapChildLocked()
	}

	var reply conversationSQLiteResult
	writeErr := c.configWrite(func() error {
		if initial == NotRunning && ctx.Err() != nil {
			return errors.New("conversation_rebind_cancelled")
		}
		if conversationApplication(context.WithoutCancel(ctx), application) != NotRunning || c.managedChildAlive() {
			return errConversationStopFailed
		}
		confirmed, writeRequest, err := store.rebindMutation(request)
		if err != nil {
			return err
		}
		target, mutation = confirmed, writeRequest
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationWriteTimeout)
		defer cancel()
		reply, err = store.run(writeCtx, mutation)
		return err
	})
	if store.hasPendingJournal() {
		writeErr = errConversationRollbackFailed
	}
	keys := reply.ConversationKeys
	result = ConversationRebindResult{ConversationKeys: keys, TargetModelAlias: target.Alias, HasMore: reply.HasMore,
		ThinkingLevel: mutation.ThinkingLevel, DirectAnthropicHistoryWarning: target.DirectAnthropicHistoryWarning && reply.HadOfficial != nil && *reply.HadOfficial}
	if initial == Running && !errors.Is(writeErr, errConversationStopFailed) && !errors.Is(writeErr, errConversationRollbackFailed) {
		restartCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		if err := restartConversationKimi(c, restartCtx); err != nil {
			result.RestartReason = stableErrorCode(err, "conversation_kimi_restart_failed")
		} else {
			result.KimiRestarted = true
		}
	}
	if writeErr != nil {
		return result, writeErr
	}
	return result, nil
}

var restartConversationKimi = func(c *Controller, ctx context.Context) error { return c.start(ctx) }

func validateConversationRebind(request ConversationRebindRequest) error {
	if request.MigratePreviousAlias == (len(request.ConversationKeys) > 0) || len(request.ConversationKeys) > conversationRebindLimit || (!request.MigratePreviousAlias && request.TargetModelAlias == "") {
		return errors.New("invalid_conversation_rebind")
	}
	seen := make(map[string]bool, len(request.ConversationKeys))
	for _, key := range request.ConversationKeys {
		if key == "" || len(key) > 2048 || strings.ContainsRune(key, 0) || seen[key] {
			return errors.New("invalid_conversation_rebind")
		}
		seen[key] = true
	}
	return nil
}

func (s conversationStore) rebindMutation(request ConversationRebindRequest) (ConversationModel, conversationSQLiteRequest, error) {
	var target ConversationModel
	mutation := conversationSQLiteRequest{Operation: "rebind", ConversationKeys: request.ConversationKeys, ModelAlias: request.TargetModelAlias}
	catalog := s.models()
	if request.TargetModelAlias != "" {
		i := slices.IndexFunc(catalog.models, func(model ConversationModel) bool { return model.Alias == request.TargetModelAlias })
		if i < 0 {
			return target, mutation, errors.New("invalid_conversation_target_model")
		}
		target = catalog.models[i]
		level := strings.ToLower(strings.TrimSpace(request.ThinkingLevel))
		if level == "" {
			level = target.DefaultEffort
		}
		if (level != "" || len(target.SupportEfforts) > 0) && !slices.Contains(target.SupportEfforts, level) {
			return target, mutation, errors.New("invalid_conversation_thinking_level")
		}
		mutation.ThinkingLevel = level
	}
	if request.MigratePreviousAlias {
		if request.TargetModelAlias == "" && request.ThinkingLevel != "" {
			return target, mutation, errors.New("invalid_conversation_thinking_level")
		}
		mutation.Limit = conversationRebindLimit
		mutation.ForeignProviders = catalog.foreignProviders
		for _, migration := range catalog.migrations {
			if request.TargetModelAlias == "" || request.TargetModelAlias == migration.TargetModelAlias {
				if request.TargetModelAlias != "" {
					migration.ThinkingLevel = mutation.ThinkingLevel
				}
				mutation.Migrations = append(mutation.Migrations, migration)
			}
		}
		if len(mutation.Migrations) == 0 {
			return target, mutation, errors.New("invalid_conversation_migration_target")
		}
	}
	return target, mutation, nil
}

var officialConversationModels = []ConversationModel{
	{Alias: "k3-agent", DisplayName: "官方 · K3", SourceKind: "official", SupportEfforts: []string{}},
	{Alias: "k3d1-agent", DisplayName: "官方 · K3.1 Pro", SourceKind: "official", SupportEfforts: []string{}},
	{Alias: "k2d8-preview", DisplayName: "官方 · K2.8 Preview", SourceKind: "official", SupportEfforts: []string{}},
}

func OfficialConversationModelName(alias string) string {
	for _, model := range officialConversationModels {
		if model.Alias == alias {
			return model.DisplayName
		}
	}
	return ""
}

const historicalKthResponsesPrefix = "byok-kth-resp/"

type conversationModelCatalog struct {
	models           []ConversationModel
	migrations       []conversationMigration
	foreignProviders []string
}

func (s conversationStore) models() conversationModelCatalog {
	catalog := conversationModelCatalog{models: slices.Clone(officialConversationModels)}
	var models []ConversationModel
	managedGateway := map[string]bool{}
	var signedIn directAccount
	store, reason := readJSONFile(filepath.Join(s.home, NativeWorkStore), maxStoreBytes)
	if reason != "" {
		return catalog
	}
	scope := AccountScope(s.home)
	if scope == "" {
		return catalog
	}
	if account, _ := selectDirectAccount(directAccountCandidates(store), sha256Hex(scope), nil); account.account != nil {
		signedIn = account
		gateway, _ := kthAccountSource(account.account)
		ownGateway := gateway != nil && !inactiveSource(gateway) && kthSourceOwned(gateway, account.account)
		active, _ := account.account["active"].(map[string]any)
		facts := providerFacts(active, nil)
		sources := make(map[string]string, len(account.sources))
		for _, source := range account.sources {
			if source["quarantined"] == true || stringField(source, "status") == "disconnected" {
				continue
			}
			kind, id := stringField(source, "kind"), stringField(source, "id")
			if id == "byok-kth" && kind == "" {
				kind = "kth"
			}
			if id != "" && (kind == "kth" || kind == "custom") {
				sources[id] = kind
			}
		}
		for _, row := range usableAccountRows(account.account, active, "") {
			fact, ok := facts[stringField(row, "providerId")]
			sourceID := stringField(row, "sourceId")
			if sourceID == "" {
				sourceID = fact.sourceID
			}
			kind := sources[sourceID]
			if ok && kind != "" && (fact.sourceID == "" || fact.sourceID == sourceID) {
				models = addConversationModel(models, row, fact.protocol, kind)
				if ownGateway && sourceID == kthSourceID && isLoopbackGateway(kthGatewayBase(fact.baseURL)) {
					managedGateway[stringField(row, "alias")] = true
				}
			}
		}
	} else {
		for _, kind := range []string{"kth", "custom"} {
			for _, candidate := range legacyProfilesOfKind(store, kind) {
				profile := candidate.profile
				if candidate.key != scope || profile["delisted"] == true || profile["quarantined"] == true || stringField(profile, "status") == "disconnected" {
					continue
				}
				facts := providerFacts(profile, nil)
				for _, row := range profileRows(profile) {
					protocol := firstNonEmpty(facts[stringField(row, "providerId")].protocol, stringField(row, "protocol"))
					if protocol == "" && kind == "kth" {
						protocol = providerType(row)
					}
					models = addConversationModel(models, row, protocol, kind)
					if kind == kthSourceKind && isLoopbackGateway(kthGatewayBase(stringField(profile, "baseUrl"))) {
						managedGateway[stringField(row, "alias")] = true
					}
				}
			}
		}
	}
	slices.SortFunc(models, func(a, b ConversationModel) int { return strings.Compare(a.Alias, b.Alias) })
	if s.mode != IntegrationModeDirect && len(managedGateway) != 0 {
		catalog.migrations, catalog.foreignProviders = s.migrations(models, managedGateway, signedIn)
	}
	catalog.models = append(models, officialConversationModels...)
	return catalog
}

func (s conversationStore) migrations(models []ConversationModel, managedGateway map[string]bool, account directAccount) ([]conversationMigration, []string) {
	directAliases, foreignProviders := s.historicalDirectAliases(account)
	var migrations []conversationMigration
	for _, model := range s.catalog {
		prefix := kthAliasPrefix
		if model.Protocol == protocol.ProtocolResponses {
			prefix = historicalKthResponsesPrefix
		}
		originalAlias := prefix + model.ID
		compatibleID := protocol.KimiCompatibleModelID(model.ID)
		compatibleAlias := kthAliasPrefix + compatibleID
		migration := conversationMigration{TargetModelAlias: compatibleAlias, Aliases: []string{originalAlias}}
		if model.CatalogID == compatibleID {
			migration.GatewayModelID = compatibleID
			migration.Aliases = append(migration.Aliases, directAliases[model.ID]...)
		} else {
			if slices.ContainsFunc(models, func(m ConversationModel) bool { return m.Alias == compatibleAlias }) {
				continue
			}
			migration.TargetModelAlias = originalAlias
			migration.Aliases = []string{compatibleAlias}
		}
		if !managedGateway[migration.TargetModelAlias] {
			continue
		}
		i := slices.IndexFunc(models, func(m ConversationModel) bool { return m.Alias == migration.TargetModelAlias })
		if i < 0 {
			continue
		}
		models[i].PreviousAlias = migration.Aliases[0]
		effort := models[i].DefaultEffort
		if effort == "" && len(models[i].SupportEfforts) > 0 {
			effort = models[i].SupportEfforts[0]
			for _, level := range models[i].SupportEfforts {
				if level != "off" {
					effort = level
					break
				}
			}
		}
		migration.ThinkingLevel, migration.SupportEfforts = effort, models[i].SupportEfforts
		migrations = append(migrations, migration)
	}
	return migrations, foreignProviders
}

// A direct alias identifies its provider by UUID, not by the upstream model name.
// Use only provider IDs tied to this account's managed custom source, including
// its retained declaration after a disconnect.
func (s conversationStore) historicalDirectAliases(account directAccount) (map[string][]string, []string) {
	managedID := ""
	if intent, err := readIntentFile(s.stateDir); err == nil && intent != nil && intent.Mode == IntegrationModeDirect &&
		intent.KimiHome == s.home && intent.AccountDigest == sha256If(account.key) {
		managedID = intent.DirectSourceID
	}
	providerOwners := map[string]string{}
	managedProviders := map[string]nativeProviderV3{}
	foreign := map[string]bool{}
	ambiguous := map[string]bool{}
	directSources := map[string]bool{}
	gateway, _ := kthAccountSource(account.account)
	ownGateway := gateway != nil && kthSourceOwned(gateway, account.account)
	add := func(provider map[string]any, sourceID string) {
		id := stringField(provider, "id")
		if id == "" {
			return
		}
		if owner, exists := providerOwners[id]; exists && owner != sourceID {
			ambiguous[id] = true
		}
		providerOwners[id] = sourceID
		if byokUUIDProvider.MatchString(id) && !(ownGateway && sourceID == kthSourceID) {
			foreign[id] = true
		}
		if !directSources[sourceID] {
			return
		}
		fact := nativeProviderV3{sourceID: sourceID, protocol: kthProviderProtocol(provider), baseURL: providerBaseURL(provider)}
		if fact.protocol == "" || fact.baseURL == "" {
			return
		}
		if previous, exists := managedProviders[id]; exists && (previous.protocol != fact.protocol ||
			normalizeDirectURL(previous.baseURL, previous.protocol) != normalizeDirectURL(fact.baseURL, fact.protocol)) {
			ambiguous[id] = true
		}
		managedProviders[id] = fact
	}
	type sourceModel struct {
		row      map[string]any
		sourceID string
	}
	var historicalModels []sourceModel
	for _, source := range account.sources {
		id := stringField(source, "id")
		if id != "" && stringField(source, "kind") == "custom" &&
			(id == managedID || stringField(source, "displayName") == DirectDisplayName) {
			directSources[id] = true
		}
		for _, section := range []map[string]any{source, mapField(source, "desired")} {
			for _, provider := range objectRows(section, "providers") {
				add(provider, id)
			}
			for _, model := range modelRows(section) {
				historicalModels = append(historicalModels, sourceModel{model, id})
			}
		}
	}
	for _, key := range []string{"active", "pending"} {
		section := mapField(account.account, key)
		for _, provider := range objectRows(section, "providers") {
			add(provider, stringField(provider, "sourceId"))
		}
		for _, model := range modelRows(section) {
			historicalModels = append(historicalModels, sourceModel{model, stringField(model, "sourceId")})
		}
	}
	aliases := map[string][]string{}
	logicalByProvider := map[string]map[string]string{}
	if s.directCatalog != nil {
		for id, fact := range managedProviders {
			if ambiguous[id] || providerOwners[id] != fact.sourceID {
				continue
			}
			byModel := map[string]string{}
			for _, provider := range s.directCatalog.Providers {
				if provider.Protocol != fact.protocol || normalizeDirectURL(provider.BaseURL, provider.Protocol) != normalizeDirectURL(fact.baseURL, fact.protocol) {
					continue
				}
				for _, model := range s.directCatalog.Models {
					if model.ProviderID == provider.ID {
						setUnambiguousModel(byModel, model.Model, model.ID)
					}
				}
			}
			logicalByProvider[id] = byModel
		}
	}
	logicalByAlias := map[string]string{}
	for _, historical := range historicalModels {
		row := historical.row
		alias := stringField(row, "alias")
		prefix, model, _ := strings.Cut(alias, "/")
		if !byokUUIDProvider.MatchString(prefix) {
			continue
		}
		id := stringField(row, "providerId")
		sourceID := firstNonEmpty(historical.sourceID, providerOwners[id])
		owned := !ambiguous[id] && providerOwners[id] == sourceID
		if !owned || !ownGateway || sourceID != kthSourceID {
			foreign[prefix] = true
		}
		if owned && model == stringField(row, "model") {
			setUnambiguousModel(logicalByAlias, alias, logicalByProvider[id][model])
		} else {
			setUnambiguousModel(logicalByAlias, alias, "")
		}
	}
	for id, byModel := range logicalByProvider {
		if !byokUUIDProvider.MatchString(id) {
			continue
		}
		for model, logical := range byModel {
			setUnambiguousModel(logicalByAlias, id+"/"+model, logical)
		}
	}
	for alias, logical := range logicalByAlias {
		if logical != "" {
			aliases[logical] = append(aliases[logical], alias)
		}
	}
	for _, values := range aliases {
		slices.Sort(values)
	}
	excluded := make([]string, 0, len(foreign))
	for id := range foreign {
		excluded = append(excluded, id)
	}
	slices.Sort(excluded)
	return aliases, excluded
}

func setUnambiguousModel(index map[string]string, key, logical string) {
	if previous, ok := index[key]; ok && previous != logical {
		logical = ""
	}
	index[key] = logical
}

func objectRows(section map[string]any, key string) []map[string]any {
	var objects []map[string]any
	rows, _ := section[key].([]any)
	for _, raw := range rows {
		if row, ok := raw.(map[string]any); ok {
			objects = append(objects, row)
		}
	}
	return objects
}

func addConversationModel(models []ConversationModel, row map[string]any, protocol, sourceKind string) []ConversationModel {
	alias := stringField(row, "alias")
	if !strings.HasPrefix(alias, "byok-") || protocol == "" || stringField(row, "model") == "" {
		return models
	}
	if slices.ContainsFunc(models, func(model ConversationModel) bool { return model.Alias == alias }) {
		return models
	}
	efforts := uniqueSorted(row["supportEfforts"])
	if efforts == nil {
		efforts = []string{}
	}
	order := []string{"off", "low", "medium", "high", "xhigh", "max"}
	rank := func(level string) int {
		if index := slices.Index(order, level); index >= 0 {
			return index
		}
		return len(order)
	}
	slices.SortStableFunc(efforts, func(a, b string) int { return rank(a) - rank(b) })
	defaultEffort := stringField(row, "defaultEffort")
	if !slices.Contains(efforts, defaultEffort) {
		defaultEffort = ""
	}
	return append(models, ConversationModel{Alias: alias, DisplayName: firstNonEmpty(stringField(row, "displayName"), stringField(row, "model")),
		Protocol: protocol, SourceKind: sourceKind, SupportEfforts: efforts, DefaultEffort: defaultEffort,
		DirectAnthropicHistoryWarning: sourceKind == "custom" && protocol == "anthropic"})
}

func (s conversationStore) journalPath() string {
	return filepath.Join(s.profile, "kimi-agent", conversationJournalName)
}

func (s conversationStore) hasPendingJournal() bool {
	if s.profile == "" {
		return false
	}
	_, err := os.Lstat(s.journalPath())
	return !errors.Is(err, os.ErrNotExist)
}

func (s conversationStore) recoverBindings(ctx context.Context) error {
	recoverCtx, cancel := context.WithTimeout(ctx, conversationRecoverTimeout)
	defer cancel()
	if _, err := s.run(recoverCtx, conversationSQLiteRequest{Operation: "recover"}); err != nil || s.hasPendingJournal() {
		return errConversationRollbackFailed
	}
	return nil
}

func (c *Controller) recoverConversationBindingsBeforeStart(ctx context.Context) error {
	store, err := c.conversationStore()
	if !store.hasPendingJournal() {
		return nil
	}
	if err != nil || conversationApplication(ctx, store.install.Application) != NotRunning {
		return errConversationRollbackFailed
	}
	return store.recoverBindings(ctx)
}

func (s conversationStore) run(ctx context.Context, request conversationSQLiteRequest) (conversationSQLiteResult, error) {
	var result conversationSQLiteResult
	if ctx.Err() != nil {
		if request.Operation == "rebind" {
			return result, errors.New("conversation_rebind_cancelled")
		}
		return result, errors.New("conversation_database_unavailable")
	}
	request.Database = filepath.Join(s.home, "agents", "main", "sessions", "hosted-logical", "conversations.sqlite")
	info, err := os.Stat(request.Database)
	if errors.Is(err, os.ErrNotExist) {
		return result, errors.New("conversation_database_missing")
	}
	if err != nil || !info.Mode().IsRegular() {
		return result, errors.New("conversation_database_unavailable")
	}
	if s.profile != "" {
		request.AgentDirectory = filepath.Join(s.profile, "kimi-agent")
	}
	input, err := json.Marshal(request)
	if err != nil {
		return result, errors.New("invalid_conversation_rebind")
	}
	for _, node := range conversationNodes(s.install, s.nodePath) {
		if ctx.Err() != nil {
			if request.Operation == "rebind" {
				return result, errors.New("conversation_rebind_cancelled")
			}
			return result, errors.New("conversation_database_unavailable")
		}
		command := execCommandContext(ctx, node.path, "-e", conversationSQLiteScript)
		hideBackgroundCommand(command)
		command.Env = node.environment(command.Environ())
		command.WaitDelay = time.Second
		command.Stdin = bytes.NewReader(input)
		output, runErr := command.Output()
		result = conversationSQLiteResult{}
		if json.Unmarshal(output, &result) == nil {
			switch result.Error {
			case "conversation_node_unavailable":
				continue
			case "conversation_rollback_failed":
				return result, errConversationRollbackFailed
			case "":
				validList := request.Operation == "list" && result.Conversations != nil
				validRebind := request.Operation == "rebind" && result.HadOfficial != nil
				validRecover := request.Operation == "recover" && result.Recovered != ""
				if runErr == nil && (validList || validRebind || validRecover) {
					return result, nil
				}
			default:
				return result, errors.New(result.Error)
			}
		}
		if ctx.Err() != nil {
			if request.Operation == "rebind" {
				if command.Process == nil {
					return result, errors.New("conversation_rebind_cancelled")
				}
				if s.hasPendingJournal() {
					recoverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationRecoverTimeout)
					recovered, recoverErr := s.run(recoverCtx, conversationSQLiteRequest{Operation: "recover"})
					cancel()
					if recoverErr == nil {
						switch recovered.Recovered {
						case "committed":
							return recovered, nil
						case "rolled_back":
							return result, errors.New("conversation_rebind_cancelled")
						}
					}
				}
				return result, errConversationRollbackFailed
			}
			return result, errors.New("conversation_database_unavailable")
		}
	}
	return result, errors.New("conversation_node_unavailable")
}

type conversationNodeCandidate struct {
	path     string
	electron bool
}

func (n conversationNodeCandidate) environment(inherited []string) []string {
	env := make([]string, 0, len(inherited)+2)
	for _, entry := range inherited {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "NODE_OPTIONS", "KIMI_DESKTOP_TOOL_NODE", "ELECTRON_NO_ASAR", "ELECTRON_RUN_AS_NODE":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "ELECTRON_NO_ASAR=1")
	if n.electron {
		env = append(env, "ELECTRON_RUN_AS_NODE=1")
	}
	return env
}

func conversationNodes(install Installation, override string) []conversationNodeCandidate {
	candidates := []conversationNodeCandidate{{path: override}}
	if override == "" {
		candidates = append(bundledConversationNodes(install, runtime.GOOS), conversationNodeCandidate{path: "node"})
	}
	var available []conversationNodeCandidate
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate.path); err == nil {
			candidate.path = path
			if !slices.Contains(available, candidate) {
				available = append(available, candidate)
			}
		}
	}
	return available
}

func bundledConversationNodes(install Installation, platform string) []conversationNodeCandidate {
	if install.Archive == "" && install.Application == "" {
		return nil
	}
	root := filepath.Dir(install.Archive)
	if install.Archive == "" {
		root = filepath.Join(filepath.Dir(install.Application), "resources")
		if platform == "darwin" {
			root = filepath.Join(filepath.Dir(filepath.Dir(install.Application)), "Resources")
		}
	}
	resources := filepath.Join(root, "resources")
	bundled := func(parts ...string) conversationNodeCandidate {
		return conversationNodeCandidate{path: filepath.Join(append([]string{resources}, parts...)...)}
	}
	if platform == "windows" {
		nodes := []conversationNodeCandidate{
			bundled("runtime", "tool-node", "node.exe"),
			bundled("runtime", "tool-node", "bin", "node.exe"),
			bundled("runtime", "node.exe"),
			bundled("tool-node", "node.exe"),
			bundled("tool-node", "bin", "node.exe"),
		}
		if install.Application != "" {
			nodes = append(nodes, conversationNodeCandidate{path: install.Application, electron: true})
		}
		return nodes
	}
	return []conversationNodeCandidate{
		bundled("runtime", "tool-node", "bin", "node"),
		bundled("runtime", "node"),
		bundled("tool-node", "bin", "node"),
		bundled("tool-node", "node"),
	}
}
