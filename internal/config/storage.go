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
	Region      string `yaml:"region,omitempty" json:"region,omitempty"`
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

// MaxStoreNameLen は置き場の名前の最大長。
// 原本を読み書きするジョブのキューは `{queue}_{store}` と置き場で修飾される。
// 修飾されうるキューで最長の `cm_detect` / `thumbnail`（9 文字）と区切りの `_` を残す。
// TestStorageQueueNameBudget が、jobs の全キュー名からこの導出を検査する。
const MaxStoreNameLen = jobq.MaxQueueNameLen - len("cm_detect") - 1

// DefaultMissingAssetAge は cleanup.missing_asset_age が 0 のときの実体無し猶予。
// 24 時間は、単発の走査揺れや DB リストア直後の一時的な不整合を、確認済みの異常と区別するための長さである。
// 削除の猶予ではないので、孤児回収の期間とは揃えない。通知の遅れを抑えるため短めにしている。
const DefaultMissingAssetAge = 24 * time.Hour

// DefaultTransferOverlap は storage.transfer.overlap を省略したときの値。
// 既定の実体無し猶予と削除 reconcile の周期から導く下限（DefaultMissingAssetAge + 2 × DeleteReconcileInterval）より長い。
// cleanup.missing_asset_age を延ばしてこの値が下限を割るときは、overlap の明示を求める。
const DefaultTransferOverlap = 48 * time.Hour

// DeleteReconcileInterval は削除 reconcile の周期。
// transfer.overlap の下限と workerspec のスケジュールで共有する。
const DeleteReconcileInterval = 15 * time.Minute

// policyConfigured は storage の置き場規則が 1 つでも書かれているかを返す。
func (c StorageConfig) policyConfigured() bool {
	return len(c.Stores) != 0 || len(c.Originals) != 0 || c.Derived != "" ||
		c.Transfer.Overlap != 0 || len(c.Transfer.Bandwidth) != 0
}

// applyPolicyDefaults は置き場規則を書いたときだけ transfer.overlap の既定値を入れる。
func (c *StorageConfig) applyPolicyDefaults() {
	if c.policyConfigured() && c.Transfer.Overlap == 0 {
		c.Transfer.Overlap = DefaultTransferOverlap
	}
}

// fsStoreNames は FS 置き場の名前をソートして返す。
func (c StorageConfig) fsStoreNames() []string {
	var names []string
	for name, store := range c.Stores {
		if store.Kind == "fs" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// FSStoreNames はファイルシステム置き場の名前を昇順で返す。
func (c StorageConfig) FSStoreNames() []string { return c.fsStoreNames() }

// setSingleFSStoreCompatibilityName は規則を、今の単一 media_dir の実装へ橋渡しする。
// FS 置き場がちょうど 1 つのときだけその名前を MediaStoreName に入れる。
// 複数の置き場を扱う worker は置き場ごとのルートを使うので、この値から desired の置き場を推測してはならない。
func (c *StorageConfig) setSingleFSStoreCompatibilityName() {
	c.MediaStoreName = ""
	if names := c.fsStoreNames(); len(names) == 1 {
		c.MediaStoreName = names[0]
	}
}

// validate は置き場の定義と desired の置き場規則を検査する。
func (c StorageConfig) validate(sites []MirakcSite, missingAssetAge time.Duration) error {
	if !c.policyConfigured() {
		return nil
	}

	errs := c.validatePolicyRequirements()
	errs = append(errs, c.validateStoreDefinitions()...)
	errs = append(errs, c.validatePolicyTargets(sites)...)
	errs = append(errs, c.validateSingleFSStore()...)
	errs = append(errs, c.validateTransfer(missingAssetAge)...)
	if len(errs) != 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// validateSingleFSStore は原本用の FS 置き場を 1 つに制限する。
func (c StorageConfig) validateSingleFSStore() []string {
	const limit = "not supported yet: workers currently write only to a single fs store"
	fsNames := c.fsStoreNames()
	if len(fsNames) != 1 {
		return []string{fmt.Sprintf("storage.stores must define exactly one fs store (%s)", limit)}
	}
	// originals の store は FS に限られる（validatePolicyTargets）。FS 置き場が 1 つなら、規則は全てそれを指す。
	only := fsNames[0]
	derived, exists := c.Stores[c.Derived]
	if exists && derived.Kind == "fs" && c.Derived != only {
		return []string{fmt.Sprintf("storage.derived %q must be the single fs store %q (%s)", c.Derived, only, limit)}
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
			if store.Endpoint != "" || store.Region != "" || store.Bucket != "" {
				errs = append(errs, fmt.Sprintf("storage.stores.%s: endpoint, region, and bucket are only valid for kind s3", name))
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
		} else if _, err := parseTransferBandwidth(bandwidth); err != nil {
			errs = append(errs, fmt.Sprintf("storage.transfer.bandwidth.%s: %v", store, err))
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
			errs = append(errs, fmt.Sprintf("storage.transfer.overlap must be greater than cleanup.missing_asset_age plus twice the delete reconcile interval (%s); set it explicitly when cleanup.missing_asset_age is raised", minimumOverlap))
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
	Site            string `json:"site,omitempty"`
	OlderThanMicros int64  `json:"older_than_us,omitempty"`
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
				match.OlderThanMicros = duration.Microseconds()
			}
			if match.Site != "" || match.OlderThanMicros != 0 {
				converted.Match = match
			}
		}
		rules = append(rules, converted)
	}
	return rules, nil
}

// OriginalRulesJSON は desired_media_store が受け取る、正規化済みの規則の JSON を返す。
// 時間はマイクロ秒に揃える。
func (c StorageConfig) OriginalRulesJSON() ([]byte, error) {
	rules, err := c.sqlOriginalRules()
	if err != nil {
		return nil, err
	}
	return json.Marshal(rules)
}

// TransferBandwidthBytesPerSecond は、指定した転送元の帯域上限を byte/s で返す。
// 未設定なら 0 を返す（無制限）。
func (c StorageConfig) TransferBandwidthBytesPerSecond(sourceStore string) (int64, error) {
	raw, ok := c.Transfer.Bandwidth[sourceStore]
	if !ok {
		return 0, nil
	}
	return parseTransferBandwidth(raw)
}

func parseTransferBandwidth(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	units := map[string]float64{
		"bps":   1.0 / 8,
		"Kbps":  1_000.0 / 8,
		"Mbps":  1_000_000.0 / 8,
		"Gbps":  1_000_000_000.0 / 8,
		"Bps":   1,
		"KBps":  1_000,
		"MBps":  1_000_000,
		"GBps":  1_000_000_000,
		"KiBps": 1 << 10,
		"MiBps": 1 << 20,
		"GiBps": 1 << 30,
	}
	unitNames := make([]string, 0, len(units))
	for unit := range units {
		unitNames = append(unitNames, unit)
	}
	sort.Slice(unitNames, func(i, j int) bool { return len(unitNames[i]) > len(unitNames[j]) })
	for _, unit := range unitNames {
		multiplier := units[unit]
		if !strings.HasSuffix(value, unit) {
			continue
		}
		number := strings.TrimSpace(strings.TrimSuffix(value, unit))
		amount, err := strconv.ParseFloat(number, 64)
		if err != nil || amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
			return 0, fmt.Errorf("must be a positive number followed by a supported bandwidth unit")
		}
		bytesPerSecond := amount * multiplier
		if bytesPerSecond < 1 || bytesPerSecond > float64(math.MaxInt64) {
			return 0, fmt.Errorf("bandwidth is outside the supported byte-per-second range")
		}
		return int64(bytesPerSecond), nil
	}
	return 0, fmt.Errorf("unsupported bandwidth unit")
}

type storagePolicyFingerprintInput struct {
	Version   int                    `json:"version"`
	Stores    map[string]StoreConfig `json:"stores"`
	Originals []sqlOriginalStoreRule `json:"originals"`
	Derived   string                 `json:"derived"`
}

// PolicyFingerprint は置き場の定義と原本・派生物の規則だけをハッシュした指紋を返す。
// transfer は含めない。帯域と overlap は転送ジョブの宛先の判断に使わないので、
// 変えても積み済みの転送ジョブを捨てさせない。
// encoding/json が map のキーを整列し、時間はマイクロ秒で表す。
func (c StorageConfig) PolicyFingerprint() (string, error) {
	rules, err := c.sqlOriginalRules()
	if err != nil {
		return "", err
	}
	stores := c.Stores
	if len(stores) == 0 {
		stores = map[string]StoreConfig{}
	}
	payload := storagePolicyFingerprintInput{
		Version:   1,
		Stores:    stores,
		Originals: rules,
		Derived:   c.Derived,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding storage policy fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
