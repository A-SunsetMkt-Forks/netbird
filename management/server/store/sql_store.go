package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	nbdns "github.com/netbirdio/netbird/dns"
	resourceTypes "github.com/netbirdio/netbird/management/server/networks/resources/types"
	routerTypes "github.com/netbirdio/netbird/management/server/networks/routers/types"
	networkTypes "github.com/netbirdio/netbird/management/server/networks/types"
	nbpeer "github.com/netbirdio/netbird/management/server/peer"
	"github.com/netbirdio/netbird/management/server/posture"
	"github.com/netbirdio/netbird/management/server/status"
	"github.com/netbirdio/netbird/management/server/telemetry"
	"github.com/netbirdio/netbird/management/server/types"
	"github.com/netbirdio/netbird/management/server/util"
	"github.com/netbirdio/netbird/route"
)

const (
	storeSqliteFileName         = "store.db"
	idQueryCondition            = "id = ?"
	keyQueryCondition           = "key = ?"
	mysqlKeyQueryCondition      = "`key` = ?"
	accountAndIDQueryCondition  = "account_id = ? and id = ?"
	accountAndIDsQueryCondition = "account_id = ? AND id IN ?"
	accountIDCondition          = "account_id = ?"
	peerNotFoundFMT             = "peer %s not found"
)

// SqlStore represents an account storage backed by a Sql DB persisted to disk
type SqlStore struct {
	db                *gorm.DB
	resourceLocks     sync.Map
	globalAccountLock sync.Mutex
	metrics           telemetry.AppMetrics
	installationPK    int
	storeEngine       types.Engine
}

type installation struct {
	ID                  uint `gorm:"primaryKey"`
	InstallationIDValue string
}

type migrationFunc func(*gorm.DB) error

// NewSqlStore creates a new SqlStore instance.
func NewSqlStore(ctx context.Context, db *gorm.DB, storeEngine types.Engine, metrics telemetry.AppMetrics, skipMigration bool) (*SqlStore, error) {
	sql, err := db.DB()
	if err != nil {
		return nil, err
	}

	conns, err := strconv.Atoi(os.Getenv("NB_SQL_MAX_OPEN_CONNS"))
	if err != nil {
		conns = runtime.NumCPU()
	}

	if storeEngine == types.SqliteStoreEngine {
		if err == nil {
			log.WithContext(ctx).Warnf("setting NB_SQL_MAX_OPEN_CONNS is not supported for sqlite, using default value 1")
		}
		conns = 1
	}

	sql.SetMaxOpenConns(conns)

	log.WithContext(ctx).Infof("Set max open db connections to %d", conns)

	if skipMigration {
		log.WithContext(ctx).Infof("skipping migration")
		return &SqlStore{db: db, storeEngine: storeEngine, metrics: metrics, installationPK: 1}, nil
	}

	if err := migratePreAuto(ctx, db); err != nil {
		return nil, fmt.Errorf("migratePreAuto: %w", err)
	}
	err = db.AutoMigrate(
		&types.SetupKey{}, &nbpeer.Peer{}, &types.User{}, &types.PersonalAccessToken{}, &types.Group{}, &types.GroupPeer{},
		&types.Account{}, &types.Policy{}, &types.PolicyRule{}, &route.Route{}, &nbdns.NameServerGroup{},
		&installation{}, &types.ExtraSettings{}, &posture.Checks{}, &nbpeer.NetworkAddress{},
		&networkTypes.Network{}, &routerTypes.NetworkRouter{}, &resourceTypes.NetworkResource{}, &types.AccountOnboarding{},
	)
	if err != nil {
		return nil, fmt.Errorf("auto migratePreAuto: %w", err)
	}
	if err := migratePostAuto(ctx, db); err != nil {
		return nil, fmt.Errorf("migratePostAuto: %w", err)
	}

	return &SqlStore{db: db, storeEngine: storeEngine, metrics: metrics, installationPK: 1}, nil
}

func GetKeyQueryCondition(s *SqlStore) string {
	if s.storeEngine == types.MysqlStoreEngine {
		return mysqlKeyQueryCondition
	}
	return keyQueryCondition
}

// AcquireGlobalLock acquires global lock across all the accounts and returns a function that releases the lock
func (s *SqlStore) AcquireGlobalLock(ctx context.Context) (unlock func()) {
	log.WithContext(ctx).Tracef("acquiring global lock")
	start := time.Now()
	s.globalAccountLock.Lock()

	unlock = func() {
		s.globalAccountLock.Unlock()
		log.WithContext(ctx).Tracef("released global lock in %v", time.Since(start))
	}

	took := time.Since(start)
	log.WithContext(ctx).Tracef("took %v to acquire global lock", took)
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountGlobalLockAcquisitionDuration(took)
	}

	return unlock
}

// AcquireWriteLockByUID acquires an ID lock for writing to a resource and returns a function that releases the lock
func (s *SqlStore) AcquireWriteLockByUID(ctx context.Context, uniqueID string) (unlock func()) {
	log.WithContext(ctx).Tracef("acquiring write lock for ID %s", uniqueID)

	start := time.Now()
	value, _ := s.resourceLocks.LoadOrStore(uniqueID, &sync.RWMutex{})
	mtx := value.(*sync.RWMutex)
	mtx.Lock()

	unlock = func() {
		mtx.Unlock()
		log.WithContext(ctx).Tracef("released write lock for ID %s in %v", uniqueID, time.Since(start))
	}

	return unlock
}

// AcquireReadLockByUID acquires an ID lock for writing to a resource and returns a function that releases the lock
func (s *SqlStore) AcquireReadLockByUID(ctx context.Context, uniqueID string) (unlock func()) {
	log.WithContext(ctx).Tracef("acquiring read lock for ID %s", uniqueID)

	start := time.Now()
	value, _ := s.resourceLocks.LoadOrStore(uniqueID, &sync.RWMutex{})
	mtx := value.(*sync.RWMutex)
	mtx.RLock()

	unlock = func() {
		mtx.RUnlock()
		log.WithContext(ctx).Tracef("released read lock for ID %s in %v", uniqueID, time.Since(start))
	}

	return unlock
}

func (s *SqlStore) SaveAccount(ctx context.Context, account *types.Account) error {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		if elapsed > 1*time.Second {
			log.WithContext(ctx).Tracef("SaveAccount for account %s exceeded 1s, took: %v", account.Id, elapsed)
		}
	}()

	// todo: remove this check after the issue is resolved
	s.checkAccountDomainBeforeSave(ctx, account.Id, account.Domain)

	generateAccountSQLTypes(account)

	for _, group := range account.GroupsG {
		group.StoreGroupPeers()
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Select(clause.Associations).Delete(account.Policies, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account.UsersG, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account)
		if result.Error != nil {
			return result.Error
		}

		result = tx.
			Session(&gorm.Session{FullSaveAssociations: true}).
			Clauses(clause.OnConflict{UpdateAll: true}).
			Create(account)
		if result.Error != nil {
			return result.Error
		}
		return nil
	})

	took := time.Since(start)
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountPersistenceDuration(took)
	}
	log.WithContext(ctx).Debugf("took %d ms to persist an account to the store", took.Milliseconds())

	return err
}

// generateAccountSQLTypes generates the GORM compatible types for the account
func generateAccountSQLTypes(account *types.Account) {
	for _, key := range account.SetupKeys {
		account.SetupKeysG = append(account.SetupKeysG, *key)
	}

	if len(account.SetupKeys) != len(account.SetupKeysG) {
		log.Warnf("SetupKeysG length mismatch for account %s", account.Id)
	}

	for id, peer := range account.Peers {
		peer.ID = id
		account.PeersG = append(account.PeersG, *peer)
	}

	for id, user := range account.Users {
		user.Id = id
		for id, pat := range user.PATs {
			pat.ID = id
			user.PATsG = append(user.PATsG, *pat)
		}
		account.UsersG = append(account.UsersG, *user)
	}

	for id, group := range account.Groups {
		group.ID = id
		group.AccountID = account.Id
		account.GroupsG = append(account.GroupsG, group)
	}

	for id, route := range account.Routes {
		route.ID = id
		account.RoutesG = append(account.RoutesG, *route)
	}

	for id, ns := range account.NameServerGroups {
		ns.ID = id
		account.NameServerGroupsG = append(account.NameServerGroupsG, *ns)
	}
}

// checkAccountDomainBeforeSave temporary method to troubleshoot an issue with domains getting blank
func (s *SqlStore) checkAccountDomainBeforeSave(ctx context.Context, accountID, newDomain string) {
	var acc types.Account
	var domain string
	result := s.db.Model(&acc).Select("domain").Where(idQueryCondition, accountID).First(&domain)
	if result.Error != nil {
		if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			log.WithContext(ctx).Errorf("error when getting account %s from the store to check domain: %s", accountID, result.Error)
		}
		return
	}
	if domain != "" && newDomain == "" {
		log.WithContext(ctx).Warnf("saving an account with empty domain when there was a domain set. Previous domain %s, Account ID: %s, Trace: %s", domain, accountID, debug.Stack())
	}
}

func (s *SqlStore) DeleteAccount(ctx context.Context, account *types.Account) error {
	start := time.Now()

	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Select(clause.Associations).Delete(account.Policies, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account.UsersG, "account_id = ?", account.Id)
		if result.Error != nil {
			return result.Error
		}

		result = tx.Select(clause.Associations).Delete(account)
		if result.Error != nil {
			return result.Error
		}

		return nil
	})

	took := time.Since(start)
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountPersistenceDuration(took)
	}
	log.WithContext(ctx).Debugf("took %d ms to delete an account to the store", took.Milliseconds())

	return err
}

func (s *SqlStore) SaveInstallationID(_ context.Context, ID string) error {
	installation := installation{InstallationIDValue: ID}
	installation.ID = uint(s.installationPK)

	return s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&installation).Error
}

func (s *SqlStore) GetInstallationID() string {
	var installation installation

	if result := s.db.First(&installation, idQueryCondition, s.installationPK); result.Error != nil {
		return ""
	}

	return installation.InstallationIDValue
}

func (s *SqlStore) SavePeer(ctx context.Context, lockStrength LockingStrength, accountID string, peer *nbpeer.Peer) error {
	// To maintain data integrity, we create a copy of the peer's to prevent unintended updates to other fields.
	peerCopy := peer.Copy()
	peerCopy.AccountID = accountID

	err := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Transaction(func(tx *gorm.DB) error {
		// check if peer exists before saving
		var peerID string
		result := tx.Model(&nbpeer.Peer{}).Select("id").Find(&peerID, accountAndIDQueryCondition, accountID, peer.ID)
		if result.Error != nil {
			return result.Error
		}

		if peerID == "" {
			return status.Errorf(status.NotFound, peerNotFoundFMT, peer.ID)
		}

		result = tx.Model(&nbpeer.Peer{}).Where(accountAndIDQueryCondition, accountID, peer.ID).Save(peerCopy)
		if result.Error != nil {
			return status.Errorf(status.Internal, "failed to save peer to store: %v", result.Error)
		}

		return nil
	})

	if err != nil {
		return err
	}

	return nil
}

func (s *SqlStore) UpdateAccountDomainAttributes(ctx context.Context, accountID string, domain string, category string, isPrimaryDomain bool) error {
	accountCopy := types.Account{
		Domain:                 domain,
		DomainCategory:         category,
		IsDomainPrimaryAccount: isPrimaryDomain,
	}

	fieldsToUpdate := []string{"domain", "domain_category", "is_domain_primary_account"}
	result := s.db.Model(&types.Account{}).
		Select(fieldsToUpdate).
		Where(idQueryCondition, accountID).
		Updates(&accountCopy)
	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to update account domain attributes to store: %v", result.Error)
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, "account %s", accountID)
	}

	return nil
}

func (s *SqlStore) SavePeerStatus(ctx context.Context, lockStrength LockingStrength, accountID, peerID string, peerStatus nbpeer.PeerStatus) error {
	var peerCopy nbpeer.Peer
	peerCopy.Status = &peerStatus

	fieldsToUpdate := []string{
		"peer_status_last_seen", "peer_status_connected",
		"peer_status_login_expired", "peer_status_required_approval",
	}
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Select(fieldsToUpdate).
		Where(accountAndIDQueryCondition, accountID, peerID).
		Updates(&peerCopy)
	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to save peer status to store: %v", result.Error)
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, peerNotFoundFMT, peerID)
	}

	return nil
}

func (s *SqlStore) SavePeerLocation(ctx context.Context, lockStrength LockingStrength, accountID string, peerWithLocation *nbpeer.Peer) error {
	// To maintain data integrity, we create a copy of the peer's location to prevent unintended updates to other fields.
	var peerCopy nbpeer.Peer
	// Since the location field has been migrated to JSON serialization,
	// updating the struct ensures the correct data format is inserted into the database.
	peerCopy.Location = peerWithLocation.Location

	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&nbpeer.Peer{}).
		Where(accountAndIDQueryCondition, accountID, peerWithLocation.ID).
		Updates(peerCopy)

	if result.Error != nil {
		return status.Errorf(status.Internal, "failed to save peer locations to store: %v", result.Error)
	}

	if result.RowsAffected == 0 {
		return status.Errorf(status.NotFound, peerNotFoundFMT, peerWithLocation.ID)
	}

	return nil
}

// SaveUsers saves the given list of users to the database.
func (s *SqlStore) SaveUsers(ctx context.Context, lockStrength LockingStrength, users []*types.User) error {
	if len(users) == 0 {
		return nil
	}

	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}, clause.OnConflict{UpdateAll: true}).Create(&users)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save users to store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save users to store")
	}
	return nil
}

// SaveUser saves the given user to the database.
func (s *SqlStore) SaveUser(ctx context.Context, lockStrength LockingStrength, user *types.User) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(user)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save user to store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save user to store")
	}
	return nil
}

// CreateGroups creates the given list of groups to the database.
func (s *SqlStore) CreateGroups(ctx context.Context, lockStrength LockingStrength, accountID string, groups []*types.Group) error {
	if len(groups) == 0 {
		return nil
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.
			Clauses(
				clause.Locking{Strength: string(lockStrength)},
				clause.OnConflict{
					Where:     clause.Where{Exprs: []clause.Expression{clause.Eq{Column: "groups.account_id", Value: accountID}}},
					UpdateAll: true,
				},
			).
			Omit(clause.Associations).
			Create(&groups)
		if result.Error != nil {
			log.WithContext(ctx).Errorf("failed to save groups to store: %v", result.Error)
			return status.Errorf(status.Internal, "failed to save groups to store")
		}

		return nil
	})
}

// UpdateGroups updates the given list of groups to the database.
func (s *SqlStore) UpdateGroups(ctx context.Context, lockStrength LockingStrength, accountID string, groups []*types.Group) error {
	if len(groups) == 0 {
		return nil
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.
			Clauses(
				clause.Locking{Strength: string(lockStrength)},
				clause.OnConflict{
					Where:     clause.Where{Exprs: []clause.Expression{clause.Eq{Column: "groups.account_id", Value: accountID}}},
					UpdateAll: true,
				},
			).
			Omit(clause.Associations).
			Create(&groups)
		if result.Error != nil {
			log.WithContext(ctx).Errorf("failed to save groups to store: %v", result.Error)
			return status.Errorf(status.Internal, "failed to save groups to store")
		}

		return nil
	})
}

// DeleteHashedPAT2TokenIDIndex is noop in SqlStore
func (s *SqlStore) DeleteHashedPAT2TokenIDIndex(hashedToken string) error {
	return nil
}

// DeleteTokenID2UserIDIndex is noop in SqlStore
func (s *SqlStore) DeleteTokenID2UserIDIndex(tokenID string) error {
	return nil
}

func (s *SqlStore) GetAccountByPrivateDomain(ctx context.Context, domain string) (*types.Account, error) {
	accountID, err := s.GetAccountIDByPrivateDomain(ctx, LockingStrengthShare, domain)
	if err != nil {
		return nil, err
	}

	// TODO:  rework to not call GetAccount
	return s.GetAccount(ctx, accountID)
}

func (s *SqlStore) GetAccountIDByPrivateDomain(ctx context.Context, lockStrength LockingStrength, domain string) (string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountID string
	result := tx.Model(&types.Account{}).Select("id").
		Where("domain = ? and is_domain_primary_account = ? and domain_category = ?",
			strings.ToLower(domain), true, types.PrivateCategory,
		).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: provided domain is not registered or is not private")
		}
		log.WithContext(ctx).Errorf("error when getting account from the store: %s", result.Error)
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountBySetupKey(ctx context.Context, setupKey string) (*types.Account, error) {
	var key types.SetupKey
	result := s.db.Select("account_id").First(&key, GetKeyQueryCondition(s), setupKey)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewSetupKeyNotFoundError(setupKey)
		}
		log.WithContext(ctx).Errorf("failed to get account by setup key from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get account by setup key from store")
	}

	if key.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, key.AccountID)
}

func (s *SqlStore) GetTokenIDByHashedToken(ctx context.Context, hashedToken string) (string, error) {
	var token types.PersonalAccessToken
	result := s.db.First(&token, "hashed_token = ?", hashedToken)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		log.WithContext(ctx).Errorf("error when getting token from the store: %s", result.Error)
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return token.ID, nil
}

func (s *SqlStore) GetUserByPATID(ctx context.Context, lockStrength LockingStrength, patID string) (*types.User, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var user types.User
	result := tx.
		Joins("JOIN personal_access_tokens ON personal_access_tokens.user_id = users.id").
		Where("personal_access_tokens.id = ?", patID).First(&user)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPATNotFoundError(patID)
		}
		log.WithContext(ctx).Errorf("failed to get token user from the store: %s", result.Error)
		return nil, status.NewGetUserFromStoreError()
	}

	return &user, nil
}

func (s *SqlStore) GetUserByUserID(ctx context.Context, lockStrength LockingStrength, userID string) (*types.User, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var user types.User
	result := tx.First(&user, idQueryCondition, userID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewUserNotFoundError(userID)
		}
		return nil, status.NewGetUserFromStoreError()
	}

	return &user, nil
}

func (s *SqlStore) DeleteUser(ctx context.Context, lockStrength LockingStrength, accountID, userID string) error {
	err := s.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: string(lockStrength)}).
			Delete(&types.PersonalAccessToken{}, "user_id = ?", userID)
		if result.Error != nil {
			return result.Error
		}

		return tx.Clauses(clause.Locking{Strength: string(lockStrength)}).
			Delete(&types.User{}, accountAndIDQueryCondition, accountID, userID).Error
	})
	if err != nil {
		log.WithContext(ctx).Errorf("failed to delete user from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete user from store")
	}

	return nil
}

func (s *SqlStore) GetAccountUsers(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*types.User, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var users []*types.User
	result := tx.Find(&users, accountIDCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "accountID not found: index lookup failed")
		}
		log.WithContext(ctx).Errorf("error when getting users from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "issue getting users from store")
	}

	return users, nil
}

func (s *SqlStore) GetAccountOwner(ctx context.Context, lockStrength LockingStrength, accountID string) (*types.User, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var user types.User
	result := tx.First(&user, "account_id = ? AND role = ?", accountID, types.UserRoleOwner)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account owner not found: index lookup failed")
		}
		return nil, status.Errorf(status.Internal, "failed to get account owner from the store")
	}

	return &user, nil
}

func (s *SqlStore) GetAccountGroups(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*types.Group, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var groups []*types.Group
	result := tx.Preload(clause.Associations).Find(&groups, accountIDCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "accountID not found: index lookup failed")
		}
		log.WithContext(ctx).Errorf("failed to get account groups from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get account groups from the store")
	}

	for _, g := range groups {
		g.LoadGroupPeers()
	}

	return groups, nil
}

func (s *SqlStore) GetResourceGroups(ctx context.Context, lockStrength LockingStrength, accountID, resourceID string) ([]*types.Group, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var groups []*types.Group

	likePattern := `%"ID":"` + resourceID + `"%`

	result := tx.
		Preload(clause.Associations).
		Where("resources LIKE ?", likePattern).
		Find(&groups)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}

	for _, g := range groups {
		g.LoadGroupPeers()
	}

	return groups, nil
}

func (s *SqlStore) GetAccountsCounter(ctx context.Context) (int64, error) {
	var count int64
	result := s.db.Model(&types.Account{}).Count(&count)
	if result.Error != nil {
		return 0, fmt.Errorf("failed to get all accounts counter: %w", result.Error)
	}

	return count, nil
}

func (s *SqlStore) GetAllAccounts(ctx context.Context) (all []*types.Account) {
	var accounts []types.Account
	result := s.db.Find(&accounts)
	if result.Error != nil {
		return all
	}

	for _, account := range accounts {
		if acc, err := s.GetAccount(ctx, account.Id); err == nil {
			all = append(all, acc)
		}
	}

	return all
}

func (s *SqlStore) GetAccountMeta(ctx context.Context, lockStrength LockingStrength, accountID string) (*types.AccountMeta, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountMeta types.AccountMeta
	result := tx.Model(&types.Account{}).
		First(&accountMeta, idQueryCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("error when getting account meta %s from the store: %s", accountID, result.Error)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountNotFoundError(accountID)
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	return &accountMeta, nil
}

// GetAccountOnboarding retrieves the onboarding information for a specific account.
func (s *SqlStore) GetAccountOnboarding(ctx context.Context, accountID string) (*types.AccountOnboarding, error) {
	var accountOnboarding types.AccountOnboarding
	result := s.db.Model(&accountOnboarding).First(&accountOnboarding, accountIDCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountOnboardingNotFoundError(accountID)
		}
		log.WithContext(ctx).Errorf("error when getting account onboarding %s from the store: %s", accountID, result.Error)
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	return &accountOnboarding, nil
}

// SaveAccountOnboarding updates the onboarding information for a specific account.
func (s *SqlStore) SaveAccountOnboarding(ctx context.Context, onboarding *types.AccountOnboarding) error {
	result := s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(onboarding)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("error when saving account onboarding %s in the store: %s", onboarding.AccountID, result.Error)
		return status.Errorf(status.Internal, "error when saving account onboarding %s in the store: %s", onboarding.AccountID, result.Error)
	}

	return nil
}

func (s *SqlStore) GetAccount(ctx context.Context, accountID string) (*types.Account, error) {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		if elapsed > 1*time.Second {
			log.WithContext(ctx).Tracef("GetAccount for account %s exceeded 1s, took: %v", accountID, elapsed)
		}
	}()

	var account types.Account
	result := s.db.Model(&account).
		Omit("GroupsG").
		Preload("UsersG.PATsG"). // have to be specifies as this is nester reference
		Preload(clause.Associations).
		First(&account, idQueryCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("error when getting account %s from the store: %s", accountID, result.Error)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountNotFoundError(accountID)
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	// we have to manually preload policy rules as it seems that gorm preloading doesn't do it for us
	for i, policy := range account.Policies {
		var rules []*types.PolicyRule
		err := s.db.Model(&types.PolicyRule{}).Find(&rules, "policy_id = ?", policy.ID).Error
		if err != nil {
			return nil, status.Errorf(status.NotFound, "rule not found")
		}
		account.Policies[i].Rules = rules
	}

	account.SetupKeys = make(map[string]*types.SetupKey, len(account.SetupKeysG))
	for _, key := range account.SetupKeysG {
		account.SetupKeys[key.Key] = key.Copy()
	}
	account.SetupKeysG = nil

	account.Peers = make(map[string]*nbpeer.Peer, len(account.PeersG))
	for _, peer := range account.PeersG {
		account.Peers[peer.ID] = peer.Copy()
	}
	account.PeersG = nil

	account.Users = make(map[string]*types.User, len(account.UsersG))
	for _, user := range account.UsersG {
		user.PATs = make(map[string]*types.PersonalAccessToken, len(user.PATs))
		for _, pat := range user.PATsG {
			user.PATs[pat.ID] = pat.Copy()
		}
		account.Users[user.Id] = user.Copy()
	}
	account.UsersG = nil

	account.Groups = make(map[string]*types.Group, len(account.GroupsG))
	for _, group := range account.GroupsG {
		account.Groups[group.ID] = group.Copy()
	}
	account.GroupsG = nil

	var groupPeers []types.GroupPeer
	s.db.Model(&types.GroupPeer{}).Where("account_id = ?", accountID).
		Find(&groupPeers)
	for _, groupPeer := range groupPeers {
		if group, ok := account.Groups[groupPeer.GroupID]; ok {
			group.Peers = append(group.Peers, groupPeer.PeerID)
		} else {
			log.WithContext(ctx).Warnf("group %s not found for group peer %s in account %s", groupPeer.GroupID, groupPeer.PeerID, accountID)
		}
	}

	account.Routes = make(map[route.ID]*route.Route, len(account.RoutesG))
	for _, route := range account.RoutesG {
		account.Routes[route.ID] = route.Copy()
	}
	account.RoutesG = nil

	account.NameServerGroups = make(map[string]*nbdns.NameServerGroup, len(account.NameServerGroupsG))
	for _, ns := range account.NameServerGroupsG {
		account.NameServerGroups[ns.ID] = ns.Copy()
	}
	account.NameServerGroupsG = nil

	return &account, nil
}

func (s *SqlStore) GetAccountByUser(ctx context.Context, userID string) (*types.Account, error) {
	var user types.User
	result := s.db.Select("account_id").First(&user, idQueryCondition, userID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	if user.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, user.AccountID)
}

func (s *SqlStore) GetAccountByPeerID(ctx context.Context, peerID string) (*types.Account, error) {
	var peer nbpeer.Peer
	result := s.db.Select("account_id").First(&peer, idQueryCondition, peerID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	if peer.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, peer.AccountID)
}

func (s *SqlStore) GetAccountByPeerPubKey(ctx context.Context, peerKey string) (*types.Account, error) {
	var peer nbpeer.Peer
	result := s.db.Select("account_id").First(&peer, GetKeyQueryCondition(s), peerKey)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return nil, status.NewGetAccountFromStoreError(result.Error)
	}

	if peer.AccountID == "" {
		return nil, status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return s.GetAccount(ctx, peer.AccountID)
}

func (s *SqlStore) GetAnyAccountID(ctx context.Context) (string, error) {
	var account types.Account
	result := s.db.WithContext(ctx).Select("id").Order("created_at desc").Limit(1).Find(&account)
	if result.Error != nil {
		return "", status.NewGetAccountFromStoreError(result.Error)
	}
	if result.RowsAffected == 0 {
		return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return account.Id, nil
}

func (s *SqlStore) GetAccountIDByPeerPubKey(ctx context.Context, peerKey string) (string, error) {
	var peer nbpeer.Peer
	var accountID string
	result := s.db.Model(&peer).Select("account_id").Where(GetKeyQueryCondition(s), peerKey).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountIDByUserID(ctx context.Context, lockStrength LockingStrength, userID string) (string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountID string
	result := tx.Model(&types.User{}).
		Select("account_id").Where(idQueryCondition, userID).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
		}
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountIDByPeerID(ctx context.Context, lockStrength LockingStrength, peerID string) (string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountID string
	result := tx.Model(&nbpeer.Peer{}).
		Select("account_id").Where(idQueryCondition, peerID).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.Errorf(status.NotFound, "peer %s account not found", peerID)
		}
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return accountID, nil
}

func (s *SqlStore) GetAccountIDBySetupKey(ctx context.Context, setupKey string) (string, error) {
	var accountID string
	result := s.db.Model(&types.SetupKey{}).Select("account_id").Where(GetKeyQueryCondition(s), setupKey).First(&accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.NewSetupKeyNotFoundError(setupKey)
		}
		log.WithContext(ctx).Errorf("failed to get account ID by setup key from store: %v", result.Error)
		return "", status.Errorf(status.Internal, "failed to get account ID by setup key from store")
	}

	if accountID == "" {
		return "", status.Errorf(status.NotFound, "account not found: index lookup failed")
	}

	return accountID, nil
}

func (s *SqlStore) GetTakenIPs(ctx context.Context, lockStrength LockingStrength, accountID string) ([]net.IP, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var ipJSONStrings []string

	// Fetch the IP addresses as JSON strings
	result := tx.Model(&nbpeer.Peer{}).
		Where("account_id = ?", accountID).
		Pluck("ip", &ipJSONStrings)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "no peers found for the account")
		}
		return nil, status.Errorf(status.Internal, "issue getting IPs from store: %s", result.Error)
	}

	// Convert the JSON strings to net.IP objects
	ips := make([]net.IP, len(ipJSONStrings))
	for i, ipJSON := range ipJSONStrings {
		var ip net.IP
		if err := json.Unmarshal([]byte(ipJSON), &ip); err != nil {
			return nil, status.Errorf(status.Internal, "issue parsing IP JSON from store")
		}
		ips[i] = ip
	}

	return ips, nil
}

func (s *SqlStore) GetPeerLabelsInAccount(ctx context.Context, lockStrength LockingStrength, accountID string, dnsLabel string) ([]string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var labels []string
	result := tx.Model(&nbpeer.Peer{}).
		Where("account_id = ? AND dns_label LIKE ?", accountID, dnsLabel+"%").
		Pluck("dns_label", &labels)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "no peers found for the account")
		}
		log.WithContext(ctx).Errorf("error when getting dns labels from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "issue getting dns labels from store: %s", result.Error)
	}

	return labels, nil
}

func (s *SqlStore) GetAccountNetwork(ctx context.Context, lockStrength LockingStrength, accountID string) (*types.Network, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountNetwork types.AccountNetwork
	if err := tx.Model(&types.Account{}).Where(idQueryCondition, accountID).First(&accountNetwork).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountNotFoundError(accountID)
		}
		return nil, status.Errorf(status.Internal, "issue getting network from store: %s", err)
	}
	return accountNetwork.Network, nil
}

func (s *SqlStore) GetPeerByPeerPubKey(ctx context.Context, lockStrength LockingStrength, peerKey string) (*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peer nbpeer.Peer
	result := tx.First(&peer, GetKeyQueryCondition(s), peerKey)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPeerNotFoundError(peerKey)
		}
		return nil, status.Errorf(status.Internal, "issue getting peer from store: %s", result.Error)
	}

	return &peer, nil
}

func (s *SqlStore) GetAccountSettings(ctx context.Context, lockStrength LockingStrength, accountID string) (*types.Settings, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountSettings types.AccountSettings
	if err := tx.Model(&types.Account{}).Where(idQueryCondition, accountID).First(&accountSettings).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "settings not found")
		}
		return nil, status.Errorf(status.Internal, "issue getting settings from store: %s", err)
	}
	return accountSettings.Settings, nil
}

func (s *SqlStore) GetAccountCreatedBy(ctx context.Context, lockStrength LockingStrength, accountID string) (string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var createdBy string
	result := tx.Model(&types.Account{}).
		Select("created_by").First(&createdBy, idQueryCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", status.NewAccountNotFoundError(accountID)
		}
		return "", status.NewGetAccountFromStoreError(result.Error)
	}

	return createdBy, nil
}

// SaveUserLastLogin stores the last login time for a user in DB.
func (s *SqlStore) SaveUserLastLogin(ctx context.Context, accountID, userID string, lastLogin time.Time) error {
	var user types.User
	result := s.db.First(&user, accountAndIDQueryCondition, accountID, userID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return status.NewUserNotFoundError(userID)
		}
		return status.NewGetUserFromStoreError()
	}

	if !lastLogin.IsZero() {
		user.LastLogin = &lastLogin
		return s.db.Save(&user).Error
	}

	return nil
}

func (s *SqlStore) GetPostureCheckByChecksDefinition(accountID string, checks *posture.ChecksDefinition) (*posture.Checks, error) {
	definitionJSON, err := json.Marshal(checks)
	if err != nil {
		return nil, err
	}

	var postureCheck posture.Checks
	err = s.db.Where("account_id = ? AND checks = ?", accountID, string(definitionJSON)).First(&postureCheck).Error
	if err != nil {
		return nil, err
	}

	return &postureCheck, nil
}

// Close closes the underlying DB connection
func (s *SqlStore) Close(_ context.Context) error {
	sql, err := s.db.DB()
	if err != nil {
		return fmt.Errorf("get db: %w", err)
	}
	return sql.Close()
}

// GetStoreEngine returns underlying store engine
func (s *SqlStore) GetStoreEngine() types.Engine {
	return s.storeEngine
}

// NewSqliteStore creates a new SQLite store.
func NewSqliteStore(ctx context.Context, dataDir string, metrics telemetry.AppMetrics, skipMigration bool) (*SqlStore, error) {
	storeStr := fmt.Sprintf("%s?cache=shared", storeSqliteFileName)
	if runtime.GOOS == "windows" {
		// Vo avoid `The process cannot access the file because it is being used by another process` on Windows
		storeStr = storeSqliteFileName
	}

	file := filepath.Join(dataDir, storeStr)
	db, err := gorm.Open(sqlite.Open(file), getGormConfig())
	if err != nil {
		return nil, err
	}

	return NewSqlStore(ctx, db, types.SqliteStoreEngine, metrics, skipMigration)
}

// NewPostgresqlStore creates a new Postgres store.
func NewPostgresqlStore(ctx context.Context, dsn string, metrics telemetry.AppMetrics, skipMigration bool) (*SqlStore, error) {
	db, err := gorm.Open(postgres.Open(dsn), getGormConfig())
	if err != nil {
		return nil, err
	}

	return NewSqlStore(ctx, db, types.PostgresStoreEngine, metrics, skipMigration)
}

// NewMysqlStore creates a new MySQL store.
func NewMysqlStore(ctx context.Context, dsn string, metrics telemetry.AppMetrics, skipMigration bool) (*SqlStore, error) {
	db, err := gorm.Open(mysql.Open(dsn+"?charset=utf8&parseTime=True&loc=Local"), getGormConfig())
	if err != nil {
		return nil, err
	}

	return NewSqlStore(ctx, db, types.MysqlStoreEngine, metrics, skipMigration)
}

func getGormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:          logger.Default.LogMode(logger.Silent),
		CreateBatchSize: 400,
	}
}

// newPostgresStore initializes a new Postgres store.
func newPostgresStore(ctx context.Context, metrics telemetry.AppMetrics, skipMigration bool) (Store, error) {
	dsn, ok := os.LookupEnv(postgresDsnEnv)
	if !ok {
		return nil, fmt.Errorf("%s is not set", postgresDsnEnv)
	}
	return NewPostgresqlStore(ctx, dsn, metrics, skipMigration)
}

// newMysqlStore initializes a new MySQL store.
func newMysqlStore(ctx context.Context, metrics telemetry.AppMetrics, skipMigration bool) (Store, error) {
	dsn, ok := os.LookupEnv(mysqlDsnEnv)
	if !ok {
		return nil, fmt.Errorf("%s is not set", mysqlDsnEnv)
	}
	return NewMysqlStore(ctx, dsn, metrics, skipMigration)
}

// NewSqliteStoreFromFileStore restores a store from FileStore and stores SQLite DB in the file located in datadir.
func NewSqliteStoreFromFileStore(ctx context.Context, fileStore *FileStore, dataDir string, metrics telemetry.AppMetrics, skipMigration bool) (*SqlStore, error) {
	store, err := NewSqliteStore(ctx, dataDir, metrics, skipMigration)
	if err != nil {
		return nil, err
	}

	err = store.SaveInstallationID(ctx, fileStore.InstallationID)
	if err != nil {
		return nil, err
	}

	for _, account := range fileStore.GetAllAccounts(ctx) {
		_, err = account.GetGroupAll()
		if err != nil {
			if err := account.AddAllGroup(false); err != nil {
				return nil, err
			}
		}

		err := store.SaveAccount(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	return store, nil
}

// NewPostgresqlStoreFromSqlStore restores a store from SqlStore and stores Postgres DB.
func NewPostgresqlStoreFromSqlStore(ctx context.Context, sqliteStore *SqlStore, dsn string, metrics telemetry.AppMetrics) (*SqlStore, error) {
	store, err := NewPostgresqlStore(ctx, dsn, metrics, false)
	if err != nil {
		return nil, err
	}

	err = store.SaveInstallationID(ctx, sqliteStore.GetInstallationID())
	if err != nil {
		return nil, err
	}

	for _, account := range sqliteStore.GetAllAccounts(ctx) {
		err := store.SaveAccount(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	return store, nil
}

// NewMysqlStoreFromSqlStore restores a store from SqlStore and stores MySQL DB.
func NewMysqlStoreFromSqlStore(ctx context.Context, sqliteStore *SqlStore, dsn string, metrics telemetry.AppMetrics) (*SqlStore, error) {
	store, err := NewMysqlStore(ctx, dsn, metrics, false)
	if err != nil {
		return nil, err
	}

	err = store.SaveInstallationID(ctx, sqliteStore.GetInstallationID())
	if err != nil {
		return nil, err
	}

	for _, account := range sqliteStore.GetAllAccounts(ctx) {
		err := store.SaveAccount(ctx, account)
		if err != nil {
			return nil, err
		}
	}

	return store, nil
}

func (s *SqlStore) GetSetupKeyBySecret(ctx context.Context, lockStrength LockingStrength, key string) (*types.SetupKey, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var setupKey types.SetupKey
	result := tx.
		First(&setupKey, GetKeyQueryCondition(s), key)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.PreconditionFailed, "setup key not found")
		}
		log.WithContext(ctx).Errorf("failed to get setup key by secret from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get setup key by secret from store")
	}
	return &setupKey, nil
}

func (s *SqlStore) IncrementSetupKeyUsage(ctx context.Context, setupKeyID string) error {
	result := s.db.Model(&types.SetupKey{}).
		Where(idQueryCondition, setupKeyID).
		Updates(map[string]interface{}{
			"used_times": gorm.Expr("used_times + 1"),
			"last_used":  time.Now(),
		})

	if result.Error != nil {
		return status.Errorf(status.Internal, "issue incrementing setup key usage count: %s", result.Error)
	}

	if result.RowsAffected == 0 {
		return status.NewSetupKeyNotFoundError(setupKeyID)
	}

	return nil
}

// AddPeerToAllGroup adds a peer to the 'All' group. Method always needs to run in a transaction
func (s *SqlStore) AddPeerToAllGroup(ctx context.Context, accountID string, peerID string) error {
	var groupID string
	_ = s.db.Model(types.Group{}).
		Select("id").
		Where("account_id = ? AND name = ?", accountID, "All").
		Limit(1).
		Scan(&groupID)

	if groupID == "" {
		return status.Errorf(status.NotFound, "group 'All' not found for account %s", accountID)
	}

	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "group_id"}, {Name: "peer_id"}},
		DoNothing: true,
	}).Create(&types.GroupPeer{
		AccountID: accountID,
		GroupID:   groupID,
		PeerID:    peerID,
	}).Error

	if err != nil {
		return status.Errorf(status.Internal, "error adding peer to group 'All': %v", err)
	}

	return nil
}

// AddPeerToGroup adds a peer to a group
func (s *SqlStore) AddPeerToGroup(ctx context.Context, accountID, peerID, groupID string) error {
	peer := &types.GroupPeer{
		AccountID: accountID,
		GroupID:   groupID,
		PeerID:    peerID,
	}

	err := s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "group_id"}, {Name: "peer_id"}},
		DoNothing: true,
	}).Create(peer).Error

	if err != nil {
		log.WithContext(ctx).Errorf("failed to add peer %s to group %s for account %s: %v", peerID, groupID, accountID, err)
		return status.Errorf(status.Internal, "failed to add peer to group")
	}

	return nil
}

// RemovePeerFromGroup removes a peer from a group
func (s *SqlStore) RemovePeerFromGroup(ctx context.Context, peerID string, groupID string) error {
	err := s.db.WithContext(ctx).
		Delete(&types.GroupPeer{}, "group_id = ? AND peer_id = ?", groupID, peerID).Error

	if err != nil {
		log.WithContext(ctx).Errorf("failed to remove peer %s from group %s: %v", peerID, groupID, err)
		return status.Errorf(status.Internal, "failed to remove peer from group")
	}

	return nil
}

// RemovePeerFromAllGroups removes a peer from all groups
func (s *SqlStore) RemovePeerFromAllGroups(ctx context.Context, peerID string) error {
	err := s.db.WithContext(ctx).
		Delete(&types.GroupPeer{}, "peer_id = ?", peerID).Error

	if err != nil {
		log.WithContext(ctx).Errorf("failed to remove peer %s from all groups: %v", peerID, err)
		return status.Errorf(status.Internal, "failed to remove peer from all groups")
	}

	return nil
}

// AddResourceToGroup adds a resource to a group. Method always needs to run n a transaction
func (s *SqlStore) AddResourceToGroup(ctx context.Context, accountId string, groupID string, resource *types.Resource) error {
	var group types.Group
	result := s.db.Where(accountAndIDQueryCondition, accountId, groupID).First(&group)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return status.NewGroupNotFoundError(groupID)
		}

		return status.Errorf(status.Internal, "issue finding group: %s", result.Error)
	}

	for _, res := range group.Resources {
		if res.ID == resource.ID {
			return nil
		}
	}

	group.Resources = append(group.Resources, *resource)

	if err := s.db.Save(&group).Error; err != nil {
		return status.Errorf(status.Internal, "issue updating group: %s", err)
	}

	return nil
}

// RemoveResourceFromGroup removes a resource from a group. Method always needs to run in a transaction
func (s *SqlStore) RemoveResourceFromGroup(ctx context.Context, accountId string, groupID string, resourceID string) error {
	var group types.Group
	result := s.db.Where(accountAndIDQueryCondition, accountId, groupID).First(&group)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return status.NewGroupNotFoundError(groupID)
		}

		return status.Errorf(status.Internal, "issue finding group: %s", result.Error)
	}

	for i, res := range group.Resources {
		if res.ID == resourceID {
			group.Resources = append(group.Resources[:i], group.Resources[i+1:]...)
			break
		}
	}

	if err := s.db.Save(&group).Error; err != nil {
		return status.Errorf(status.Internal, "issue updating group: %s", err)
	}

	return nil
}

// GetPeerGroups retrieves all groups assigned to a specific peer in a given account.
func (s *SqlStore) GetPeerGroups(ctx context.Context, lockStrength LockingStrength, accountId string, peerId string) ([]*types.Group, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var groups []*types.Group
	query := tx.
		Joins("JOIN group_peers ON group_peers.group_id = groups.id").
		Where("group_peers.peer_id = ?", peerId).
		Preload(clause.Associations).
		Find(&groups)

	if query.Error != nil {
		return nil, query.Error
	}

	for _, group := range groups {
		group.LoadGroupPeers()
	}

	return groups, nil
}

// GetPeerGroupIDs retrieves all group IDs assigned to a specific peer in a given account.
func (s *SqlStore) GetPeerGroupIDs(ctx context.Context, lockStrength LockingStrength, accountId string, peerId string) ([]string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var groupIDs []string
	query := tx.
		Model(&types.GroupPeer{}).
		Where("account_id = ? AND peer_id = ?", accountId, peerId).
		Pluck("group_id", &groupIDs)

	if query.Error != nil {
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return nil, status.Errorf(status.NotFound, "no groups found for peer %s in account %s", peerId, accountId)
		}
		log.WithContext(ctx).Errorf("failed to get group IDs for peer %s in account %s: %v", peerId, accountId, query.Error)
		return nil, status.Errorf(status.Internal, "failed to get group IDs for peer from store")
	}

	return groupIDs, nil
}

// GetAccountPeers retrieves peers for an account.
func (s *SqlStore) GetAccountPeers(ctx context.Context, lockStrength LockingStrength, accountID, nameFilter, ipFilter string) ([]*nbpeer.Peer, error) {
	var peers []*nbpeer.Peer
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}
	query := tx.Where(accountIDCondition, accountID)

	if nameFilter != "" {
		query = query.Where("name LIKE ?", "%"+nameFilter+"%")
	}
	if ipFilter != "" {
		query = query.Where("ip LIKE ?", "%"+ipFilter+"%")
	}

	if err := query.Find(&peers).Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get peers from store")
	}

	return peers, nil
}

// GetUserPeers retrieves peers for a user.
func (s *SqlStore) GetUserPeers(ctx context.Context, lockStrength LockingStrength, accountID, userID string) ([]*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peers []*nbpeer.Peer

	// Exclude peers added via setup keys, as they are not user-specific and have an empty user_id.
	if userID == "" {
		return peers, nil
	}

	result := tx.
		Find(&peers, "account_id = ? AND user_id = ?", accountID, userID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get peers from store")
	}

	return peers, nil
}

func (s *SqlStore) AddPeerToAccount(ctx context.Context, lockStrength LockingStrength, peer *nbpeer.Peer) error {
	if err := s.db.Create(peer).Error; err != nil {
		return status.Errorf(status.Internal, "issue adding peer to account: %s", err)
	}

	return nil
}

// GetPeerByID retrieves a peer by its ID and account ID.
func (s *SqlStore) GetPeerByID(ctx context.Context, lockStrength LockingStrength, accountID, peerID string) (*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peer *nbpeer.Peer
	result := tx.
		First(&peer, accountAndIDQueryCondition, accountID, peerID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPeerNotFoundError(peerID)
		}
		return nil, status.Errorf(status.Internal, "failed to get peer from store")
	}

	return peer, nil
}

// GetPeersByIDs retrieves peers by their IDs and account ID.
func (s *SqlStore) GetPeersByIDs(ctx context.Context, lockStrength LockingStrength, accountID string, peerIDs []string) (map[string]*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peers []*nbpeer.Peer
	result := tx.Find(&peers, accountAndIDsQueryCondition, accountID, peerIDs)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get peers by ID's from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get peers by ID's from the store")
	}

	peersMap := make(map[string]*nbpeer.Peer)
	for _, peer := range peers {
		peersMap[peer.ID] = peer
	}

	return peersMap, nil
}

// GetAccountPeersWithExpiration retrieves a list of peers that have login expiration enabled and added by a user.
func (s *SqlStore) GetAccountPeersWithExpiration(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peers []*nbpeer.Peer
	result := tx.
		Where("login_expiration_enabled = ? AND user_id IS NOT NULL AND user_id != ''", true).
		Find(&peers, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers with expiration from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get peers with expiration from store")
	}

	return peers, nil
}

// GetAccountPeersWithInactivity retrieves a list of peers that have login expiration enabled and added by a user.
func (s *SqlStore) GetAccountPeersWithInactivity(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peers []*nbpeer.Peer
	result := tx.
		Where("inactivity_expiration_enabled = ? AND user_id IS NOT NULL AND user_id != ''", true).
		Find(&peers, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get peers with inactivity from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get peers with inactivity from store")
	}

	return peers, nil
}

// GetAllEphemeralPeers retrieves all peers with Ephemeral set to true across all accounts, optimized for batch processing.
func (s *SqlStore) GetAllEphemeralPeers(ctx context.Context, lockStrength LockingStrength) ([]*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var allEphemeralPeers, batchPeers []*nbpeer.Peer
	result := tx.
		Where("ephemeral = ?", true).
		FindInBatches(&batchPeers, 1000, func(tx *gorm.DB, batch int) error {
			allEphemeralPeers = append(allEphemeralPeers, batchPeers...)
			return nil
		})

	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to retrieve ephemeral peers: %s", result.Error)
		return nil, fmt.Errorf("failed to retrieve ephemeral peers")
	}

	return allEphemeralPeers, nil
}

// DeletePeer removes a peer from the store.
func (s *SqlStore) DeletePeer(ctx context.Context, lockStrength LockingStrength, accountID string, peerID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&nbpeer.Peer{}, accountAndIDQueryCondition, accountID, peerID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete peer from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete peer from store")
	}

	if result.RowsAffected == 0 {
		return status.NewPeerNotFoundError(peerID)
	}

	return nil
}

func (s *SqlStore) IncrementNetworkSerial(ctx context.Context, lockStrength LockingStrength, accountId string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Model(&types.Account{}).Where(idQueryCondition, accountId).Update("network_serial", gorm.Expr("network_serial + 1"))
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to increment network serial count in store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to increment network serial count in store")
	}
	return nil
}

func (s *SqlStore) ExecuteInTransaction(ctx context.Context, operation func(store Store) error) error {
	startTime := time.Now()
	tx := s.db.Begin()
	if tx.Error != nil {
		return tx.Error
	}
	repo := s.withTx(tx)
	err := operation(repo)
	if err != nil {
		tx.Rollback()
		return err
	}

	err = tx.Commit().Error

	log.WithContext(ctx).Tracef("transaction took %v", time.Since(startTime))
	if s.metrics != nil {
		s.metrics.StoreMetrics().CountTransactionDuration(time.Since(startTime))
	}

	return err
}

func (s *SqlStore) withTx(tx *gorm.DB) Store {
	return &SqlStore{
		db:          tx,
		storeEngine: s.storeEngine,
	}
}

func (s *SqlStore) GetDB() *gorm.DB {
	return s.db
}

func (s *SqlStore) GetAccountDNSSettings(ctx context.Context, lockStrength LockingStrength, accountID string) (*types.DNSSettings, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountDNSSettings types.AccountDNSSettings
	result := tx.Model(&types.Account{}).
		First(&accountDNSSettings, idQueryCondition, accountID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewAccountNotFoundError(accountID)
		}
		log.WithContext(ctx).Errorf("failed to get dns settings from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get dns settings from store")
	}
	return &accountDNSSettings.DNSSettings, nil
}

// AccountExists checks whether an account exists by the given ID.
func (s *SqlStore) AccountExists(ctx context.Context, lockStrength LockingStrength, id string) (bool, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var accountID string
	result := tx.Model(&types.Account{}).
		Select("id").First(&accountID, idQueryCondition, id)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, result.Error
	}

	return accountID != "", nil
}

// GetAccountDomainAndCategory retrieves the Domain and DomainCategory fields for an account based on the given accountID.
func (s *SqlStore) GetAccountDomainAndCategory(ctx context.Context, lockStrength LockingStrength, accountID string) (string, string, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var account types.Account
	result := tx.Model(&types.Account{}).Select("domain", "domain_category").
		Where(idQueryCondition, accountID).First(&account)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", "", status.Errorf(status.NotFound, "account not found")
		}
		return "", "", status.Errorf(status.Internal, "failed to get domain category from store: %v", result.Error)
	}

	return account.Domain, account.DomainCategory, nil
}

// GetGroupByID retrieves a group by ID and account ID.
func (s *SqlStore) GetGroupByID(ctx context.Context, lockStrength LockingStrength, accountID, groupID string) (*types.Group, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var group *types.Group
	result := tx.Preload(clause.Associations).First(&group, accountAndIDQueryCondition, accountID, groupID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewGroupNotFoundError(groupID)
		}
		log.WithContext(ctx).Errorf("failed to get group from store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get group from store")
	}

	group.LoadGroupPeers()

	return group, nil
}

// GetGroupByName retrieves a group by name and account ID.
func (s *SqlStore) GetGroupByName(ctx context.Context, lockStrength LockingStrength, accountID, groupName string) (*types.Group, error) {
	tx := s.db

	var group types.Group

	// TODO: This fix is accepted for now, but if we need to handle this more frequently
	// we may need to reconsider changing the types.
	query := tx.Preload(clause.Associations)

	result := query.
		Model(&types.Group{}).
		Joins("LEFT JOIN group_peers ON group_peers.group_id = groups.id").
		Where("groups.account_id = ? AND groups.name = ?", accountID, groupName).
		Group("groups.id").
		Order("COUNT(group_peers.peer_id) DESC").
		Limit(1).
		First(&group)
	if err := result.Error; err != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewGroupNotFoundError(groupName)
		}
		log.WithContext(ctx).Errorf("failed to get group by name from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get group by name from store")
	}

	group.LoadGroupPeers()

	return &group, nil
}

// GetGroupsByIDs retrieves groups by their IDs and account ID.
func (s *SqlStore) GetGroupsByIDs(ctx context.Context, lockStrength LockingStrength, accountID string, groupIDs []string) (map[string]*types.Group, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var groups []*types.Group
	result := tx.Preload(clause.Associations).Find(&groups, accountAndIDsQueryCondition, accountID, groupIDs)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get groups by ID's from store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get groups by ID's from store")
	}

	groupsMap := make(map[string]*types.Group)
	for _, group := range groups {
		group.LoadGroupPeers()
		groupsMap[group.ID] = group
	}

	return groupsMap, nil
}

// CreateGroup creates a group in the store.
func (s *SqlStore) CreateGroup(ctx context.Context, lockStrength LockingStrength, group *types.Group) error {
	if group == nil {
		return status.Errorf(status.InvalidArgument, "group is nil")
	}

	if err := s.db.Omit(clause.Associations).Create(group).Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save group to store: %v", err)
		return status.Errorf(status.Internal, "failed to save group to store")
	}

	return nil
}

// UpdateGroup updates a group in the store.
func (s *SqlStore) UpdateGroup(ctx context.Context, lockStrength LockingStrength, group *types.Group) error {
	if group == nil {
		return status.Errorf(status.InvalidArgument, "group is nil")
	}

	if err := s.db.Omit(clause.Associations).Save(group).Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save group to store: %v", err)
		return status.Errorf(status.Internal, "failed to save group to store")
	}

	return nil
}

// DeleteGroup deletes a group from the database.
func (s *SqlStore) DeleteGroup(ctx context.Context, lockStrength LockingStrength, accountID, groupID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Select(clause.Associations).
		Delete(&types.Group{}, accountAndIDQueryCondition, accountID, groupID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete group from store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to delete group from store")
	}

	if result.RowsAffected == 0 {
		return status.NewGroupNotFoundError(groupID)
	}

	return nil
}

// DeleteGroups deletes groups from the database.
func (s *SqlStore) DeleteGroups(ctx context.Context, strength LockingStrength, accountID string, groupIDs []string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(strength)}).
		Select(clause.Associations).
		Delete(&types.Group{}, accountAndIDsQueryCondition, accountID, groupIDs)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete groups from store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to delete groups from store")
	}

	return nil
}

// GetAccountPolicies retrieves policies for an account.
func (s *SqlStore) GetAccountPolicies(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*types.Policy, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var policies []*types.Policy
	result := tx.
		Preload(clause.Associations).Find(&policies, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get policies from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get policies from store")
	}

	return policies, nil
}

// GetPolicyByID retrieves a policy by its ID and account ID.
func (s *SqlStore) GetPolicyByID(ctx context.Context, lockStrength LockingStrength, accountID, policyID string) (*types.Policy, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var policy *types.Policy

	result := tx.Preload(clause.Associations).
		First(&policy, accountAndIDQueryCondition, accountID, policyID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewPolicyNotFoundError(policyID)
		}
		log.WithContext(ctx).Errorf("failed to get policy from store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get policy from store")
	}

	return policy, nil
}

func (s *SqlStore) CreatePolicy(ctx context.Context, lockStrength LockingStrength, policy *types.Policy) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Create(policy)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to create policy in store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to create policy in store")
	}

	return nil
}

// SavePolicy saves a policy to the database.
func (s *SqlStore) SavePolicy(ctx context.Context, lockStrength LockingStrength, policy *types.Policy) error {
	result := s.db.Session(&gorm.Session{FullSaveAssociations: true}).
		Clauses(clause.Locking{Strength: string(lockStrength)}).Save(policy)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save policy to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save policy to store")
	}
	return nil
}

func (s *SqlStore) DeletePolicy(ctx context.Context, lockStrength LockingStrength, accountID, policyID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("policy_id = ?", policyID).Delete(&types.PolicyRule{}).Error; err != nil {
			return fmt.Errorf("delete policy rules: %w", err)
		}

		result := tx.Clauses(clause.Locking{Strength: string(lockStrength)}).
			Where(accountAndIDQueryCondition, accountID, policyID).
			Delete(&types.Policy{})

		if err := result.Error; err != nil {
			log.WithContext(ctx).Errorf("failed to delete policy from store: %s", err)
			return status.Errorf(status.Internal, "failed to delete policy from store")
		}

		if result.RowsAffected == 0 {
			return status.NewPolicyNotFoundError(policyID)
		}

		return nil
	})
}

// GetAccountPostureChecks retrieves posture checks for an account.
func (s *SqlStore) GetAccountPostureChecks(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*posture.Checks, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var postureChecks []*posture.Checks
	result := tx.Find(&postureChecks, accountIDCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get posture checks from store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get posture checks from store")
	}

	return postureChecks, nil
}

// GetPostureChecksByID retrieves posture checks by their ID and account ID.
func (s *SqlStore) GetPostureChecksByID(ctx context.Context, lockStrength LockingStrength, accountID, postureChecksID string) (*posture.Checks, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var postureCheck *posture.Checks
	result := tx.
		First(&postureCheck, accountAndIDQueryCondition, accountID, postureChecksID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPostureChecksNotFoundError(postureChecksID)
		}
		log.WithContext(ctx).Errorf("failed to get posture check from store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get posture check from store")
	}

	return postureCheck, nil
}

// GetPostureChecksByIDs retrieves posture checks by their IDs and account ID.
func (s *SqlStore) GetPostureChecksByIDs(ctx context.Context, lockStrength LockingStrength, accountID string, postureChecksIDs []string) (map[string]*posture.Checks, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var postureChecks []*posture.Checks
	result := tx.Find(&postureChecks, accountAndIDsQueryCondition, accountID, postureChecksIDs)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get posture checks by ID's from store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get posture checks by ID's from store")
	}

	postureChecksMap := make(map[string]*posture.Checks)
	for _, postureCheck := range postureChecks {
		postureChecksMap[postureCheck.ID] = postureCheck
	}

	return postureChecksMap, nil
}

// SavePostureChecks saves a posture checks to the database.
func (s *SqlStore) SavePostureChecks(ctx context.Context, lockStrength LockingStrength, postureCheck *posture.Checks) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(postureCheck)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save posture checks to store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save posture checks to store")
	}

	return nil
}

// DeletePostureChecks deletes a posture checks from the database.
func (s *SqlStore) DeletePostureChecks(ctx context.Context, lockStrength LockingStrength, accountID, postureChecksID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&posture.Checks{}, accountAndIDQueryCondition, accountID, postureChecksID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete posture checks from store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to delete posture checks from store")
	}

	if result.RowsAffected == 0 {
		return status.NewPostureChecksNotFoundError(postureChecksID)
	}

	return nil
}

// GetAccountRoutes retrieves network routes for an account.
func (s *SqlStore) GetAccountRoutes(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*route.Route, error) {
	var routes []*route.Route
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Find(&routes, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get routes from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get routes from store")
	}

	return routes, nil
}

// GetRouteByID retrieves a route by its ID and account ID.
func (s *SqlStore) GetRouteByID(ctx context.Context, lockStrength LockingStrength, accountID string, routeID string) (*route.Route, error) {
	var route *route.Route
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&route, accountAndIDQueryCondition, accountID, routeID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewRouteNotFoundError(routeID)
		}
		log.WithContext(ctx).Errorf("failed to get route from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get route from store")
	}

	return route, nil
}

// SaveRoute saves a route to the database.
func (s *SqlStore) SaveRoute(ctx context.Context, lockStrength LockingStrength, route *route.Route) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(route)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save route to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save route to store")
	}

	return nil
}

// DeleteRoute deletes a route from the database.
func (s *SqlStore) DeleteRoute(ctx context.Context, lockStrength LockingStrength, accountID, routeID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&route.Route{}, accountAndIDQueryCondition, accountID, routeID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete route from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete route from store")
	}

	if result.RowsAffected == 0 {
		return status.NewRouteNotFoundError(routeID)
	}

	return nil
}

// GetAccountSetupKeys retrieves setup keys for an account.
func (s *SqlStore) GetAccountSetupKeys(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*types.SetupKey, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var setupKeys []*types.SetupKey
	result := tx.
		Find(&setupKeys, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get setup keys from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get setup keys from store")
	}

	return setupKeys, nil
}

// GetSetupKeyByID retrieves a setup key by its ID and account ID.
func (s *SqlStore) GetSetupKeyByID(ctx context.Context, lockStrength LockingStrength, accountID, setupKeyID string) (*types.SetupKey, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var setupKey *types.SetupKey
	result := tx.Clauses(clause.Locking{Strength: string(lockStrength)}).
		First(&setupKey, accountAndIDQueryCondition, accountID, setupKeyID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewSetupKeyNotFoundError(setupKeyID)
		}
		log.WithContext(ctx).Errorf("failed to get setup key from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get setup key from store")
	}

	return setupKey, nil
}

// SaveSetupKey saves a setup key to the database.
func (s *SqlStore) SaveSetupKey(ctx context.Context, lockStrength LockingStrength, setupKey *types.SetupKey) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(setupKey)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save setup key to store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to save setup key to store")
	}

	return nil
}

// DeleteSetupKey deletes a setup key from the database.
func (s *SqlStore) DeleteSetupKey(ctx context.Context, lockStrength LockingStrength, accountID, keyID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Delete(&types.SetupKey{}, accountAndIDQueryCondition, accountID, keyID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete setup key from store: %s", result.Error)
		return status.Errorf(status.Internal, "failed to delete setup key from store")
	}

	if result.RowsAffected == 0 {
		return status.NewSetupKeyNotFoundError(keyID)
	}

	return nil
}

// GetAccountNameServerGroups retrieves name server groups for an account.
func (s *SqlStore) GetAccountNameServerGroups(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*nbdns.NameServerGroup, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var nsGroups []*nbdns.NameServerGroup
	result := tx.Find(&nsGroups, accountIDCondition, accountID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get name server groups from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get name server groups from store")
	}

	return nsGroups, nil
}

// GetNameServerGroupByID retrieves a name server group by its ID and account ID.
func (s *SqlStore) GetNameServerGroupByID(ctx context.Context, lockStrength LockingStrength, accountID, nsGroupID string) (*nbdns.NameServerGroup, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var nsGroup *nbdns.NameServerGroup
	result := tx.
		First(&nsGroup, accountAndIDQueryCondition, accountID, nsGroupID)
	if err := result.Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, status.NewNameServerGroupNotFoundError(nsGroupID)
		}
		log.WithContext(ctx).Errorf("failed to get name server group from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get name server group from store")
	}

	return nsGroup, nil
}

// SaveNameServerGroup saves a name server group to the database.
func (s *SqlStore) SaveNameServerGroup(ctx context.Context, lockStrength LockingStrength, nameServerGroup *nbdns.NameServerGroup) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(nameServerGroup)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save name server group to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save name server group to store")
	}
	return nil
}

// DeleteNameServerGroup deletes a name server group from the database.
func (s *SqlStore) DeleteNameServerGroup(ctx context.Context, lockStrength LockingStrength, accountID, nsGroupID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Delete(&nbdns.NameServerGroup{}, accountAndIDQueryCondition, accountID, nsGroupID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete name server group from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete name server group from store")
	}

	if result.RowsAffected == 0 {
		return status.NewNameServerGroupNotFoundError(nsGroupID)
	}

	return nil
}

// SaveDNSSettings saves the DNS settings to the store.
func (s *SqlStore) SaveDNSSettings(ctx context.Context, lockStrength LockingStrength, accountID string, settings *types.DNSSettings) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&types.Account{}).
		Where(idQueryCondition, accountID).Updates(&types.AccountDNSSettings{DNSSettings: *settings})
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save dns settings to store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to save dns settings to store")
	}

	if result.RowsAffected == 0 {
		return status.NewAccountNotFoundError(accountID)
	}

	return nil
}

// SaveAccountSettings stores the account settings in DB.
func (s *SqlStore) SaveAccountSettings(ctx context.Context, lockStrength LockingStrength, accountID string, settings *types.Settings) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Model(&types.Account{}).
		Select("*").Where(idQueryCondition, accountID).Updates(&types.AccountSettings{Settings: settings})
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save account settings to store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to save account settings to store")
	}

	if result.RowsAffected == 0 {
		return status.NewAccountNotFoundError(accountID)
	}

	return nil
}

func (s *SqlStore) GetAccountNetworks(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*networkTypes.Network, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var networks []*networkTypes.Network
	result := tx.Find(&networks, accountIDCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get networks from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get networks from store")
	}

	return networks, nil
}

func (s *SqlStore) GetNetworkByID(ctx context.Context, lockStrength LockingStrength, accountID, networkID string) (*networkTypes.Network, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var network *networkTypes.Network
	result := tx.
		First(&network, accountAndIDQueryCondition, accountID, networkID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewNetworkNotFoundError(networkID)
		}

		log.WithContext(ctx).Errorf("failed to get network from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network from store")
	}

	return network, nil
}

func (s *SqlStore) SaveNetwork(ctx context.Context, lockStrength LockingStrength, network *networkTypes.Network) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(network)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save network to store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to save network to store")
	}

	return nil
}

func (s *SqlStore) DeleteNetwork(ctx context.Context, lockStrength LockingStrength, accountID, networkID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&networkTypes.Network{}, accountAndIDQueryCondition, accountID, networkID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete network from store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to delete network from store")
	}

	if result.RowsAffected == 0 {
		return status.NewNetworkNotFoundError(networkID)
	}

	return nil
}

func (s *SqlStore) GetNetworkRoutersByNetID(ctx context.Context, lockStrength LockingStrength, accountID, netID string) ([]*routerTypes.NetworkRouter, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netRouters []*routerTypes.NetworkRouter
	result := tx.
		Find(&netRouters, "account_id = ? AND network_id = ?", accountID, netID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get network routers from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network routers from store")
	}

	return netRouters, nil
}

func (s *SqlStore) GetNetworkRoutersByAccountID(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*routerTypes.NetworkRouter, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netRouters []*routerTypes.NetworkRouter
	result := tx.
		Find(&netRouters, accountIDCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get network routers from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network routers from store")
	}

	return netRouters, nil
}

func (s *SqlStore) GetNetworkRouterByID(ctx context.Context, lockStrength LockingStrength, accountID, routerID string) (*routerTypes.NetworkRouter, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netRouter *routerTypes.NetworkRouter
	result := tx.
		First(&netRouter, accountAndIDQueryCondition, accountID, routerID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewNetworkRouterNotFoundError(routerID)
		}
		log.WithContext(ctx).Errorf("failed to get network router from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network router from store")
	}

	return netRouter, nil
}

func (s *SqlStore) SaveNetworkRouter(ctx context.Context, lockStrength LockingStrength, router *routerTypes.NetworkRouter) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(router)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save network router to store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to save network router to store")
	}

	return nil
}

func (s *SqlStore) DeleteNetworkRouter(ctx context.Context, lockStrength LockingStrength, accountID, routerID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&routerTypes.NetworkRouter{}, accountAndIDQueryCondition, accountID, routerID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete network router from store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to delete network router from store")
	}

	if result.RowsAffected == 0 {
		return status.NewNetworkRouterNotFoundError(routerID)
	}

	return nil
}

func (s *SqlStore) GetNetworkResourcesByNetID(ctx context.Context, lockStrength LockingStrength, accountID, networkID string) ([]*resourceTypes.NetworkResource, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netResources []*resourceTypes.NetworkResource
	result := tx.
		Find(&netResources, "account_id = ? AND network_id = ?", accountID, networkID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get network resources from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network resources from store")
	}

	return netResources, nil
}

func (s *SqlStore) GetNetworkResourcesByAccountID(ctx context.Context, lockStrength LockingStrength, accountID string) ([]*resourceTypes.NetworkResource, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netResources []*resourceTypes.NetworkResource
	result := tx.
		Find(&netResources, accountIDCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get network resources from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network resources from store")
	}

	return netResources, nil
}

func (s *SqlStore) GetNetworkResourceByID(ctx context.Context, lockStrength LockingStrength, accountID, resourceID string) (*resourceTypes.NetworkResource, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netResources *resourceTypes.NetworkResource
	result := tx.
		First(&netResources, accountAndIDQueryCondition, accountID, resourceID)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewNetworkResourceNotFoundError(resourceID)
		}
		log.WithContext(ctx).Errorf("failed to get network resource from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network resource from store")
	}

	return netResources, nil
}

func (s *SqlStore) GetNetworkResourceByName(ctx context.Context, lockStrength LockingStrength, accountID, resourceName string) (*resourceTypes.NetworkResource, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var netResources *resourceTypes.NetworkResource
	result := tx.
		First(&netResources, "account_id = ? AND name = ?", accountID, resourceName)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewNetworkResourceNotFoundError(resourceName)
		}
		log.WithContext(ctx).Errorf("failed to get network resource from store: %v", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get network resource from store")
	}

	return netResources, nil
}

func (s *SqlStore) SaveNetworkResource(ctx context.Context, lockStrength LockingStrength, resource *resourceTypes.NetworkResource) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(resource)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to save network resource to store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to save network resource to store")
	}

	return nil
}

func (s *SqlStore) DeleteNetworkResource(ctx context.Context, lockStrength LockingStrength, accountID, resourceID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&resourceTypes.NetworkResource{}, accountAndIDQueryCondition, accountID, resourceID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to delete network resource from store: %v", result.Error)
		return status.Errorf(status.Internal, "failed to delete network resource from store")
	}

	if result.RowsAffected == 0 {
		return status.NewNetworkResourceNotFoundError(resourceID)
	}

	return nil
}

// GetPATByHashedToken returns a PersonalAccessToken by its hashed token.
func (s *SqlStore) GetPATByHashedToken(ctx context.Context, lockStrength LockingStrength, hashedToken string) (*types.PersonalAccessToken, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var pat types.PersonalAccessToken
	result := tx.First(&pat, "hashed_token = ?", hashedToken)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPATNotFoundError(hashedToken)
		}
		log.WithContext(ctx).Errorf("failed to get pat by hash from the store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get pat by hash from store")
	}

	return &pat, nil
}

// GetPATByID retrieves a personal access token by its ID and user ID.
func (s *SqlStore) GetPATByID(ctx context.Context, lockStrength LockingStrength, userID string, patID string) (*types.PersonalAccessToken, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var pat types.PersonalAccessToken
	result := tx.
		First(&pat, "id = ? AND user_id = ?", patID, userID)
	if err := result.Error; err != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, status.NewPATNotFoundError(patID)
		}
		log.WithContext(ctx).Errorf("failed to get pat from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get pat from store")
	}

	return &pat, nil
}

// GetUserPATs retrieves personal access tokens for a user.
func (s *SqlStore) GetUserPATs(ctx context.Context, lockStrength LockingStrength, userID string) ([]*types.PersonalAccessToken, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var pats []*types.PersonalAccessToken
	result := tx.Find(&pats, "user_id = ?", userID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to get user pat's from the store: %s", err)
		return nil, status.Errorf(status.Internal, "failed to get user pat's from store")
	}

	return pats, nil
}

// MarkPATUsed marks a personal access token as used.
func (s *SqlStore) MarkPATUsed(ctx context.Context, lockStrength LockingStrength, patID string) error {
	patCopy := types.PersonalAccessToken{
		LastUsed: util.ToPtr(time.Now().UTC()),
	}

	fieldsToUpdate := []string{"last_used"}
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Select(fieldsToUpdate).
		Where(idQueryCondition, patID).Updates(&patCopy)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to mark pat as used: %s", result.Error)
		return status.Errorf(status.Internal, "failed to mark pat as used")
	}

	if result.RowsAffected == 0 {
		return status.NewPATNotFoundError(patID)
	}

	return nil
}

// SavePAT saves a personal access token to the database.
func (s *SqlStore) SavePAT(ctx context.Context, lockStrength LockingStrength, pat *types.PersonalAccessToken) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).Save(pat)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to save pat to the store: %s", err)
		return status.Errorf(status.Internal, "failed to save pat to store")
	}

	return nil
}

// DeletePAT deletes a personal access token from the database.
func (s *SqlStore) DeletePAT(ctx context.Context, lockStrength LockingStrength, userID, patID string) error {
	result := s.db.Clauses(clause.Locking{Strength: string(lockStrength)}).
		Delete(&types.PersonalAccessToken{}, "user_id = ? AND id = ?", userID, patID)
	if err := result.Error; err != nil {
		log.WithContext(ctx).Errorf("failed to delete pat from the store: %s", err)
		return status.Errorf(status.Internal, "failed to delete pat from store")
	}

	if result.RowsAffected == 0 {
		return status.NewPATNotFoundError(patID)
	}

	return nil
}

func (s *SqlStore) GetPeerByIP(ctx context.Context, lockStrength LockingStrength, accountID string, ip net.IP) (*nbpeer.Peer, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	jsonValue := fmt.Sprintf(`"%s"`, ip.String())

	var peer nbpeer.Peer
	result := tx.
		First(&peer, "account_id = ? AND ip = ?", accountID, jsonValue)
	if result.Error != nil {
		// no logging here
		return nil, status.Errorf(status.Internal, "failed to get peer from store")
	}

	return &peer, nil
}

func (s *SqlStore) GetPeerIdByLabel(ctx context.Context, lockStrength LockingStrength, accountID string, hostname string) (string, error) {
	tx := s.db.WithContext(ctx)
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peerID string
	result := tx.Model(&nbpeer.Peer{}).
		Select("id").
		// Where(" = ?", hostname).
		Where("account_id = ? AND dns_label = ?", accountID, hostname).
		Limit(1).
		Scan(&peerID)

	if peerID == "" {
		return "", gorm.ErrRecordNotFound
	}

	return peerID, result.Error
}

func (s *SqlStore) CountAccountsByPrivateDomain(ctx context.Context, domain string) (int64, error) {
	var count int64
	result := s.db.Model(&types.Account{}).
		Where("domain = ? AND domain_category = ?",
			strings.ToLower(domain), types.PrivateCategory,
		).Count(&count)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to count accounts by private domain %s: %s", domain, result.Error)
		return 0, status.Errorf(status.Internal, "failed to count accounts by private domain")
	}

	return count, nil
}

func (s *SqlStore) GetAccountGroupPeers(ctx context.Context, lockStrength LockingStrength, accountID string) (map[string]map[string]struct{}, error) {
	tx := s.db
	if lockStrength != LockingStrengthNone {
		tx = tx.Clauses(clause.Locking{Strength: string(lockStrength)})
	}

	var peers []types.GroupPeer
	result := tx.Find(&peers, accountIDCondition, accountID)
	if result.Error != nil {
		log.WithContext(ctx).Errorf("failed to get account group peers from store: %s", result.Error)
		return nil, status.Errorf(status.Internal, "failed to get account group peers from store")
	}

	groupPeers := make(map[string]map[string]struct{})
	for _, peer := range peers {
		if _, exists := groupPeers[peer.GroupID]; !exists {
			groupPeers[peer.GroupID] = make(map[string]struct{})
		}
		groupPeers[peer.GroupID][peer.PeerID] = struct{}{}
	}

	return groupPeers, nil
}
