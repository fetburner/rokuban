package config

// 置き場定義と desired 置き場規則の設定。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/mediapath"
)

// StoreConfig は 1 つの物理置き場の接続定義。
type StoreConfig struct {
	Kind        string `yaml:"kind" json:"kind"`
	InternalURL string `yaml:"internal_url,omitempty" json:"internal_url,omitempty"`
	Endpoint    string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Bucket      string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
}

// OriginalStoreRule は原本の置き場を選ぶ 1 規則。
// Match が nil または空なら無条件に一致する。
type OriginalStoreRule struct {
	Match *OriginalStoreMatch `yaml:"match,omitempty" json:"match,omitempty"`
	Store string              `yaml:"store" json:"store"`
}

// OriginalStoreMatch は原本規則の任意条件。
// OlderThan は放送終了から現在までの経過時間で、d は 24 時間として解釈する。
type OriginalStoreMatch struct {
	Site      string `yaml:"site,omitempty" json:"site,omitempty"`
	OlderThan string `yaml:"older_than,omitempty" json:"older_than,omitempty"`
}

// StorageTransferConfig は置き場間転送の設定。
type StorageTransferConfig struct {
	Bandwidth map[string]string `yaml:"bandwidth,omitempty" json:"bandwidth,omitempty"`
	Overlap   time.Duration     `yaml:"overlap" json:"overlap"`
}

// MaxStoreNameLen は store 修飾を付けた queue が River の 64 文字制限に収まる上限。
// 現行の store-bound queue で最長の `cm_detect` / `thumbnail` と区切りの `_` を残す。
const MaxStoreNameLen = jobq.MaxQueueNameLen - len("cm_detect") - 1

// DefaultMissingAssetAge is the observation window used when cleanup.missing_asset_age is 0.
const DefaultMissingAssetAge = 24 * time.Hour

// DeleteReconcileInterval は削除 reconcile の周期。
// transfer.overlap の下限と workerspec のスケジュールで共有する。
const DeleteReconcileInterval = 15 * time.Minute

// setSingleFSStoreCompatibilityName bridges the policy configuration to the current
// single-root media_dir implementation. Multi-store workers use store-qualified roots
// in a later integration step and must not infer a desired store from this field.
func (c *StorageConfig) setSingleFSStoreCompatibilityName() {
	c.MediaStoreName = ""
	var fsNames []string
	for name, store := range c.Stores {
		if store.Kind == "fs" {
			fsNames = append(fsNames, name)
		}
	}
	if len(fsNames) == 1 {
		c.MediaStoreName = fsNames[0]
	}
}

// validate checks storage definitions and desired-store rules.
func (c StorageConfig) validate(sites []MirakcSite, missingAssetAge time.Duration) error {
	policyConfigured := len(c.Stores) != 0 || len(c.Originals) != 0 || c.Derived != "" ||
		c.Transfer.Overlap != 0 || len(c.Transfer.Bandwidth) != 0
	if !policyConfigured {
		return nil
	}

	errs := c.validatePolicyRequirements()
	errs = append(errs, c.validateStoreDefinitions()...)
	errs = append(errs, c.validatePolicyTargets(sites)...)
	errs = append(errs, c.validateTransfer(missingAssetAge)...)
	if len(errs) != 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (c StorageConfig) validatePolicyRequirements() []string {
	var errs []string
	if len(c.Stores) == 0 {
		errs = append(errs, "storage.stores is required when storage policy is configured")
	}
	if len(c.Originals) == 0 {
		errs = append(errs, "storage.originals must contain an unconditional final rule")
	}
	if c.Derived == "" {
		errs = append(errs, "storage.derived is required when storage policy is configured")
	}
	return errs
}

func (c StorageConfig) validateStoreDefinitions() []string {
	var errs []string
	storeNames := make([]string, 0, len(c.Stores))
	for name := range c.Stores {
		storeNames = append(storeNames, name)
	}
	sort.Strings(storeNames)
	for _, name := range storeNames {
		store := c.Stores[name]
		if !mediapath.ValidStoreName(name) {
			errs = append(errs, fmt.Sprintf("storage.stores key %q must contain only lowercase ASCII letters, digits and hyphens", name))
		}
		if len(name) > MaxStoreNameLen {
			errs = append(errs, fmt.Sprintf("storage.stores key %q exceeds %d characters (queue names are limited to 64)", name, MaxStoreNameLen))
		}
		switch store.Kind {
		case "fs":
			if store.Endpoint != "" || store.Bucket != "" {
				errs = append(errs, fmt.Sprintf("storage.stores.%s: endpoint and bucket are only valid for kind s3", name))
			}
		case "s3":
			if store.Bucket == "" {
				errs = append(errs, fmt.Sprintf("storage.stores.%s.bucket is required for kind s3", name))
			}
			if store.Endpoint != "" {
				if err := validateStorageURL(store.Endpoint); err != nil {
					errs = append(errs, fmt.Sprintf("storage.stores.%s.endpoint: %v", name, err))
				}
			}
		default:
			errs = append(errs, fmt.Sprintf("storage.stores.%s.kind must be fs or s3", name))
		}
		if store.InternalURL != "" {
			if err := validateStorageURL(store.InternalURL); err != nil {
				errs = append(errs, fmt.Sprintf("storage.stores.%s.internal_url: %v", name, err))
			}
		}
	}
	return errs
}

func (c StorageConfig) validatePolicyTargets(sites []MirakcSite) []string {
	var errs []string
	if c.Derived != "" {
		if _, ok := c.Stores[c.Derived]; !ok {
			errs = append(errs, fmt.Sprintf("storage.derived references unknown store %q", c.Derived))
		}
	}
	siteNames := make(map[string]struct{}, len(sites))
	for _, site := range sites {
		siteNames[site.Site] = struct{}{}
	}
	for i, rule := range c.Originals {
		label := fmt.Sprintf("storage.originals[%d]", i)
		store, ok := c.Stores[rule.Store]
		if rule.Store == "" {
			errs = append(errs, label+".store is required")
		} else if !ok {
			errs = append(errs, fmt.Sprintf("%s.store references unknown store %q", label, rule.Store))
		} else if store.Kind != "fs" {
			errs = append(errs, fmt.Sprintf("%s.store %q must reference an fs store", label, rule.Store))
		}

		unconditional := rule.Match == nil || (rule.Match.Site == "" && rule.Match.OlderThan == "")
		if i == len(c.Originals)-1 {
			if !unconditional {
				errs = append(errs, label+" must be unconditional (the final rule is the fallback)")
			}
		} else if unconditional {
			errs = append(errs, fmt.Sprintf("%s is unconditional but only the final rule may be unconditional", label))
		}
		if rule.Match == nil {
			continue
		}
		if rule.Match.Site != "" {
			if _, ok := siteNames[rule.Match.Site]; !ok {
				errs = append(errs, fmt.Sprintf("%s.match.site references unknown site %q", label, rule.Match.Site))
			}
		}
		if rule.Match.OlderThan != "" {
			if _, err := parseOlderThan(rule.Match.OlderThan); err != nil {
				errs = append(errs, fmt.Sprintf("%s.match.older_than: %v", label, err))
			}
		}
	}
	return errs
}

func (c StorageConfig) validateTransfer(missingAssetAge time.Duration) []string {
	var errs []string
	bandwidthStores := make([]string, 0, len(c.Transfer.Bandwidth))
	for store := range c.Transfer.Bandwidth {
		bandwidthStores = append(bandwidthStores, store)
	}
	sort.Strings(bandwidthStores)
	for _, store := range bandwidthStores {
		bandwidth := c.Transfer.Bandwidth[store]
		if _, ok := c.Stores[store]; !ok {
			errs = append(errs, fmt.Sprintf("storage.transfer.bandwidth references unknown store %q", store))
		}
		if strings.TrimSpace(bandwidth) == "" {
			errs = append(errs, fmt.Sprintf("storage.transfer.bandwidth.%s must not be empty", store))
		}
	}

	if missingAssetAge <= 0 {
		missingAssetAge = DefaultMissingAssetAge
	}
	deleteWindow := 2 * DeleteReconcileInterval
	if missingAssetAge > time.Duration(math.MaxInt64)-deleteWindow {
		errs = append(errs, "storage.transfer.overlap cannot exceed cleanup.missing_asset_age plus twice the delete reconcile interval")
	} else {
		minimumOverlap := missingAssetAge + deleteWindow
		if c.Transfer.Overlap <= minimumOverlap {
			errs = append(errs, fmt.Sprintf("storage.transfer.overlap must be greater than cleanup.missing_asset_age plus twice the delete reconcile interval (%s)", minimumOverlap))
		}
	}
	return errs
}

func validateStorageURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("must be an absolute http or https URL: %w", err)
	}
	if !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be an absolute http or https URL with a host, no user information, query, or fragment")
	}
	return nil
}

func parseOlderThan(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(value, "d"), 64)
		if err != nil || math.IsNaN(days) || math.IsInf(days, 0) || days <= 0 || days > float64(math.MaxInt64)/float64(24*time.Hour) {
			return 0, fmt.Errorf("must be a positive duration such as 30d or 720h")
		}
		duration := time.Duration(math.Round(days * float64(24*time.Hour)))
		if duration <= 0 {
			return 0, fmt.Errorf("must be a positive duration such as 30d or 720h")
		}
		if duration%time.Microsecond != 0 {
			return 0, fmt.Errorf("must have microsecond precision")
		}
		return duration, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("must be a positive duration such as 30d or 720h")
	}
	if duration%time.Microsecond != 0 {
		return 0, fmt.Errorf("must have microsecond precision")
	}
	return duration, nil
}

type sqlOriginalStoreRule struct {
	Match *sqlOriginalStoreMatch `json:"match,omitempty"`
	Store string                 `json:"store"`
}

type sqlOriginalStoreMatch struct {
	Site           string `json:"site,omitempty"`
	OlderThanNanos int64  `json:"older_than_ns,omitempty"`
}

func (c StorageConfig) sqlOriginalRules() ([]sqlOriginalStoreRule, error) {
	rules := make([]sqlOriginalStoreRule, 0, len(c.Originals))
	for i, rule := range c.Originals {
		converted := sqlOriginalStoreRule{Store: rule.Store}
		if rule.Match != nil {
			match := &sqlOriginalStoreMatch{Site: rule.Match.Site}
			if rule.Match.OlderThan != "" {
				duration, err := parseOlderThan(rule.Match.OlderThan)
				if err != nil {
					return nil, fmt.Errorf("storage.originals[%d].match.older_than: %w", i, err)
				}
				match.OlderThanNanos = int64(duration)
			}
			if match.Site != "" || match.OlderThanNanos != 0 {
				converted.Match = match
			}
		}
		rules = append(rules, converted)
	}
	return rules, nil
}

// OriginalRulesJSON returns the normalized rules argument accepted by desired_media_store.
func (c StorageConfig) OriginalRulesJSON() ([]byte, error) {
	rules, err := c.sqlOriginalRules()
	if err != nil {
		return nil, err
	}
	return json.Marshal(rules)
}

type storagePolicyFingerprintInput struct {
	Version   int                        `json:"version"`
	Stores    map[string]StoreConfig     `json:"stores"`
	Originals []sqlOriginalStoreRule     `json:"originals"`
	Derived   string                     `json:"derived"`
	Transfer  storagePolicyTransferInput `json:"transfer"`
}

type storagePolicyTransferInput struct {
	Bandwidth map[string]string `json:"bandwidth,omitempty"`
	OverlapNS int64             `json:"overlap_ns"`
}

// PolicyFingerprint hashes only storage definitions and placement/transfer policy.
// encoding/json sorts map keys, while duration values are represented as nanoseconds.
func (c StorageConfig) PolicyFingerprint() (string, error) {
	rules, err := c.sqlOriginalRules()
	if err != nil {
		return "", err
	}
	stores := c.Stores
	if len(stores) == 0 {
		stores = map[string]StoreConfig{}
	}
	bandwidth := c.Transfer.Bandwidth
	if len(bandwidth) == 0 {
		bandwidth = nil
	}
	payload := storagePolicyFingerprintInput{
		Version:   1,
		Stores:    stores,
		Originals: rules,
		Derived:   c.Derived,
		Transfer: storagePolicyTransferInput{
			Bandwidth: bandwidth,
			OverlapNS: int64(c.Transfer.Overlap),
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding storage policy fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// normalizedOlderThanSeconds is kept here to make the SQL conversion explicit and
// avoid copying the desired-store predicate into Go.
