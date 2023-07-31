package storage

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/XDagger/xdagpool/pool"
	"github.com/XDagger/xdagpool/util"

	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()

type RedisClient struct {
	client *redis.Client
	prefix string
}

type BlockData struct {
	Height         int64    `json:"height"`
	Timestamp      int64    `json:"timestamp"`
	Difficulty     int64    `json:"difficulty"`
	TotalShares    int64    `json:"shares"`
	Uncle          bool     `json:"uncle"`
	UncleHeight    int64    `json:"uncleHeight"`
	Orphan         bool     `json:"orphan"`
	Hash           string   `json:"hash"`
	Nonce          string   `json:"-"`
	ENonce1        string   `json:"-"`
	ENonce2        string   `json:"-"`
	CoinBaseValue  *big.Int `json:"-"`
	BlkTotalFee    *big.Int `json:"-"`
	Reward         *big.Int `json:"-"`
	ExtraReward    *big.Int `json:"-"`
	ImmatureReward string   `json:"-"`
	RewardString   string   `json:"reward"`
	RoundHeight    int64    `json:"-"`
	candidateKey   string
	immatureKey    string
}

type HashRateStatsData struct {
	SharesCount uint64 `json:"sharesCount"`
	TotalWorks  uint64 `json:"totalWorks"`
}

func (b *BlockData) RewardInSatoshi() int64 {
	return b.Reward.Int64()
}

func (b *BlockData) serializeHash() string {
	if len(b.Hash) > 0 {
		return b.Hash
	} else {
		return "0x0"
	}
}

func (b *BlockData) RoundKey() string {
	return join(b.RoundHeight, b.Hash)
}

func (b *BlockData) key() string {
	return join(
		b.UncleHeight,
		b.Orphan,
		b.Nonce,
		b.serializeHash(),
		b.Timestamp,
		b.Difficulty,
		b.TotalShares,
		b.CoinBaseValue,
		b.BlkTotalFee,
		b.Reward)
}

type Miner struct {
	LastBeat  int64 `json:"lastBeat"`
	HR        int64 `json:"hr"`
	Offline   bool  `json:"offline"`
	startedAt int64
}

type Worker struct {
	Miner
	TotalHR int64 `json:"hr2"`
}

func NewRedisClient(cfg *pool.StorageConfig, prefix string) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Endpoint,
		Password: cfg.Password,
		DB:       int(cfg.Database),
		PoolSize: cfg.PoolSize,
	})
	return &RedisClient{client: client, prefix: prefix}
}

func NewRedisFailoverClient(cfg *pool.StorageConfigFailover, prefix string) *RedisClient {
	client := redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:    cfg.MasterName,
		SentinelAddrs: cfg.SentinelEndpoints,
		Password:      cfg.Password,
		DB:            int(cfg.Database),
		PoolSize:      cfg.PoolSize,
	})
	return &RedisClient{client: client, prefix: prefix}
}

func (r *RedisClient) Client() *redis.Client {
	return r.client
}

func (r *RedisClient) Check() (string, error) {
	return r.client.Ping(ctx).Result()
}

func (r *RedisClient) BgSave() (string, error) {
	return r.client.BgSave(ctx).Result()
}

// Always returns list of addresses. If Redis fails it will return empty list.
func (r *RedisClient) GetBlacklist() ([]string, error) {
	cmd := r.client.SMembers(ctx, r.formatKey("blacklist"))
	if cmd.Err() != nil {
		return []string{}, cmd.Err()
	}
	return cmd.Val(), nil
}

// Always returns list of IPs. If Redis fails it will return empty list.
func (r *RedisClient) GetWhitelist() ([]string, error) {
	cmd := r.client.SMembers(ctx, r.formatKey("whitelist"))
	if cmd.Err() != nil {
		return []string{}, cmd.Err()
	}
	return cmd.Val(), nil
}

func (r *RedisClient) WriteNodeState(id string, height uint32, diff *big.Int) error {
	tx := r.client.TxPipeline()
	now := util.MakeTimestamp() / 1000
	tx.HSet(ctx, r.formatKey("nodes"), join(id, "name"), id)
	tx.HSet(ctx, r.formatKey("nodes"), join(id, "height"), strconv.FormatUint(uint64(height), 10))
	tx.HSet(ctx, r.formatKey("nodes"), join(id, "difficulty"), diff.String())
	tx.HSet(ctx, r.formatKey("nodes"), join(id, "lastBeat"), strconv.FormatInt(now, 10))
	_, err := tx.Exec(ctx)
	return err
}

func (r *RedisClient) GetNodeStates() ([]map[string]interface{}, error) {
	cmd := r.client.HGetAll(ctx, r.formatKey("nodes"))
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	m := make(map[string]map[string]interface{})
	for key, value := range cmd.Val() {
		parts := strings.Split(key, ":")
		if val, ok := m[parts[0]]; ok {
			val[parts[1]] = value
		} else {
			node := make(map[string]interface{})
			node[parts[1]] = value
			m[parts[0]] = node
		}
	}
	v := make([]map[string]interface{}, len(m))
	i := 0
	for _, value := range m {
		v[i] = value
		i++
	}
	return v, nil
}

func (r *RedisClient) checkPoWExist(height uint64, params []string) (bool, error) {
	r.client.ZRemRangeByScore(ctx, r.formatKey("pow"), "-inf", fmt.Sprint("(", height-3))
	val, err := r.client.ZAdd(ctx, r.formatKey("pow"), redis.Z{Score: float64(height), Member: strings.Join(params, ":")}).Result()
	return val == 0, err
}

func (r *RedisClient) WriteShare(login, id string, params []string, diff int64, height uint64, window time.Duration) (bool, error) {
	exist, err := r.checkPoWExist(height, params)
	if err != nil {
		return false, err
	}
	if exist {
		return true, nil
	}

	tx := r.client.TxPipeline()
	// defer tx.Close()
	ms := util.MakeTimestamp()
	ts := ms / 1000

	r.writeShare(tx, ms, ts, login, id, diff, window)
	tx.HIncrBy(ctx, r.formatKey("stats"), "roundShares", diff)

	_, err = tx.Exec(ctx)
	return false, err
}

func (r *RedisClient) WriteInvalidShare(ms, ts int64, login, id string, diff int64) error {
	cmd := r.client.ZAdd(ctx, r.formatKey("invalidhashrate"), redis.Z{Score: float64(ts), Member: join(diff, login, id, ms)})
	if cmd.Err() != nil {
		return cmd.Err()
	}
	return nil
}

func (r *RedisClient) WriteRejectShare(ms, ts int64, login, id string, diff int64) error {
	cmd := r.client.ZAdd(ctx, r.formatKey("rejecthashrate"), redis.Z{Score: float64(ts), Member: join(diff, login, id, ms)})
	if cmd.Err() != nil {
		return cmd.Err()
	}
	return nil
}

func (r *RedisClient) WriteBlock(login, id string, params []string, diff, roundDiff int64, height uint64,
	coinBaseValue int64, blkTotalFee int64, window time.Duration) (bool, error) {
	exist, err := r.checkPoWExist(height, params)
	if err != nil {
		return false, err
	}
	if exist {
		return true, nil
	}
	tx := r.client.TxPipeline()
	// defer tx.Close()

	ms := util.MakeTimestamp()
	ts := ms / 1000

	r.writeShare(tx, ms, ts, login, id, diff, window)
	tx.HSet(ctx, r.formatKey("stats"), "lastBlockFound", strconv.FormatInt(ts, 10))
	tx.HDel(ctx, r.formatKey("stats"), "roundShares")
	tx.ZIncrBy(ctx, r.formatKey("finders"), 1, login)
	tx.HIncrBy(ctx, r.formatKey("miners", login), "blocksFound", 1)
	tx.Rename(ctx, r.formatKey("shares", "roundCurrent"), r.formatRound(int64(height), params[0]))
	tx.HGetAll(ctx, r.formatRound(int64(height), params[0]))

	cmds, err := tx.Exec(ctx)
	if err != nil {
		return false, err
	} else {
		sharesMap, _ := cmds[10].(*redis.MapStringStringCmd).Result()
		totalShares := int64(0)
		for _, v := range sharesMap {
			n, _ := strconv.ParseInt(v, 10, 64)
			totalShares += n
		}
		hashHex := strings.Join(params, ":")
		s := join(hashHex, ts, roundDiff, totalShares, coinBaseValue, blkTotalFee)
		cmd := r.client.ZAdd(ctx, r.formatKey("blocks", "candidates"), redis.Z{Score: float64(height), Member: s})
		return false, cmd.Err()
	}
}

func (r *RedisClient) writeShare(tx redis.Pipeliner, ms, ts int64, login, id string, diff int64, expire time.Duration) {
	tx.HIncrBy(ctx, r.formatKey("shares", "roundCurrent"), login, diff)
	tx.ZAdd(ctx, r.formatKey("hashrate"), redis.Z{Score: float64(ts), Member: join(diff, login, id, ms)})
	tx.ZAdd(ctx, r.formatKey("hashrate", login), redis.Z{Score: float64(ts), Member: join(diff, id, ms)})
	tx.Expire(ctx, r.formatKey("hashrate", login), expire) // Will delete hashrates for miners that gone
	tx.HSet(ctx, r.formatKey("miners", login), "lastShare", strconv.FormatInt(ts, 10))
}

func (r *RedisClient) formatKey(args ...interface{}) string {
	return join(r.prefix, join(args...))
}

func (r *RedisClient) formatRound(height int64, nonce string) string {
	return r.formatKey("shares", "round"+strconv.FormatInt(height, 10), nonce)
}

func join(args ...interface{}) string {
	s := make([]string, len(args))
	for i, v := range args {
		switch x := v.(type) {
		case string:
			s[i] = x
		case int64:
			s[i] = strconv.FormatInt(x, 10)
		case uint64:
			s[i] = strconv.FormatUint(x, 10)
		case float64:
			s[i] = strconv.FormatFloat(x, 'f', 0, 64)
		case bool:
			if x {
				s[i] = "1"
			} else {
				s[i] = "0"
			}
		case *big.Int:
			if x != nil {
				s[i] = x.String()
			} else {
				s[i] = "0"
			}
		default:
			panic("Invalid type specified for conversion")
		}
	}
	return strings.Join(s, ":")
}

func (r *RedisClient) GetCandidates(maxHeight int64) ([]*BlockData, error) {
	option := redis.ZRangeBy{Min: "0", Max: strconv.FormatInt(maxHeight, 10)}
	cmd := r.client.ZRangeByScoreWithScores(ctx, r.formatKey("blocks", "candidates"), &option)
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	return convertCandidateResults(cmd), nil
}

func (r *RedisClient) GetImmatureBlocks(maxHeight int64) ([]*BlockData, error) {
	option := redis.ZRangeBy{Min: "0", Max: strconv.FormatInt(maxHeight, 10)}
	cmd := r.client.ZRangeByScoreWithScores(ctx, r.formatKey("blocks", "immature"), &option)
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	return convertBlockResults(cmd), nil
}

func (r *RedisClient) GetRoundShares(height int64, nonce string) (map[string]int64, error) {
	result := make(map[string]int64)
	cmd := r.client.HGetAll(ctx, r.formatRound(height, nonce))
	if cmd.Err() != nil {
		return nil, cmd.Err()
	}
	sharesMap, _ := cmd.Result()
	for login, v := range sharesMap {
		n, _ := strconv.ParseInt(v, 10, 64)
		result[login] = n
	}
	return result, nil
}

func (r *RedisClient) GetPayees() ([]string, error) {
	payees := make(map[string]struct{})
	var result []string
	var c uint64

	for {
		var keys []string
		var err error
		keys, c, err = r.client.Scan(ctx, c, r.formatKey("miners", "*"), 100).Result()
		if err != nil {
			return nil, err
		}
		for _, row := range keys {
			login := strings.Split(row, ":")[2]
			payees[login] = struct{}{}
		}
		if c == 0 {
			break
		}
	}
	for login := range payees {
		result = append(result, login)
	}
	return result, nil
}

func (r *RedisClient) GetBalance(login string) (int64, error) {
	cmd := r.client.HGet(ctx, r.formatKey("miners", login), "balance")
	if cmd.Err() == redis.Nil {
		return 0, nil
	} else if cmd.Err() != nil {
		return 0, cmd.Err()
	}
	return cmd.Int64()
}

func (r *RedisClient) LockPayouts(login string, amount int64) error {
	key := r.formatKey("payments", "lock")
	result := r.client.SetNX(ctx, key, join(login, amount), 0).Val()
	if !result {
		return fmt.Errorf("unable to acquire lock '%s'", key)
	}
	return nil
}

func (r *RedisClient) UnlockPayouts() error {
	key := r.formatKey("payments", "lock")
	_, err := r.client.Del(ctx, key).Result()
	return err
}

func (r *RedisClient) IsPayoutsLocked() (bool, error) {
	_, err := r.client.Get(ctx, r.formatKey("payments", "lock")).Result()
	if err == redis.Nil {
		return false, nil
	} else if err != nil {
		return false, err
	} else {
		return true, nil
	}
}

type PendingPayment struct {
	Timestamp int64  `json:"timestamp"`
	Amount    int64  `json:"amount"`
	Address   string `json:"login"`
}

func (r *RedisClient) GetPendingPayments() []*PendingPayment {
	raw := r.client.ZRevRangeWithScores(ctx, r.formatKey("payments", "pending"), 0, -1)
	var result []*PendingPayment
	for _, v := range raw.Val() {
		// timestamp -> "address:amount"
		payment := PendingPayment{}
		payment.Timestamp = int64(v.Score)
		fields := strings.Split(v.Member.(string), ":")
		payment.Address = fields[0]
		payment.Amount, _ = strconv.ParseInt(fields[1], 10, 64)
		result = append(result, &payment)
	}
	return result
}

// Deduct miner's balance for payment
func (r *RedisClient) UpdateBalance(login string, amount int64) error {
	tx := r.client.TxPipeline()
	// defer tx.Close()

	ts := util.MakeTimestamp() / 1000

	tx.HIncrBy(ctx, r.formatKey("miners", login), "balance", (amount * -1))
	tx.HIncrBy(ctx, r.formatKey("miners", login), "pending", amount)
	tx.HIncrBy(ctx, r.formatKey("finances"), "balance", (amount * -1))
	tx.HIncrBy(ctx, r.formatKey("finances"), "pending", amount)
	tx.ZAdd(ctx, r.formatKey("payments", "pending"), redis.Z{Score: float64(ts), Member: join(login, amount)})

	_, err := tx.Exec(ctx)
	return err
}

func (r *RedisClient) RollbackBalance(login string, amount int64) error {
	tx := r.client.TxPipeline()
	// defer tx.Close()
	tx.HIncrBy(ctx, r.formatKey("miners", login), "balance", amount)
	tx.HIncrBy(ctx, r.formatKey("miners", login), "pending", (amount * -1))
	tx.HIncrBy(ctx, r.formatKey("finances"), "balance", amount)
	tx.HIncrBy(ctx, r.formatKey("finances"), "pending", (amount * -1))
	tx.ZRem(ctx, r.formatKey("payments", "pending"), join(login, amount))

	_, err := tx.Exec(ctx)
	return err
}

func (r *RedisClient) WritePayment(login, txHash string, amount int64) error {
	tx := r.client.TxPipeline()
	// defer tx.Close()

	ts := util.MakeTimestamp() / 1000

	tx.HIncrBy(ctx, r.formatKey("miners", login), "pending", (amount * -1))
	tx.HIncrBy(ctx, r.formatKey("miners", login), "paid", amount)
	tx.HIncrBy(ctx, r.formatKey("finances"), "pending", (amount * -1))
	tx.HIncrBy(ctx, r.formatKey("finances"), "paid", amount)
	tx.ZAdd(ctx, r.formatKey("payments", "all"), redis.Z{Score: float64(ts), Member: join(txHash, login, amount)})
	tx.ZAdd(ctx, r.formatKey("payments", login), redis.Z{Score: float64(ts), Member: join(txHash, amount)})
	tx.ZRem(ctx, r.formatKey("payments", "pending"), join(login, amount))

	tx.Del(ctx, r.formatKey("payments", "lock"))
	_, err := tx.Exec(ctx)
	return err
}

func (r *RedisClient) WriteImmatureBlock(block *BlockData, roundRewards map[string]int64) error {
	tx := r.client.TxPipeline()
	// defer tx.Close()
	r.writeImmatureBlock(tx, block)
	total := int64(0)
	for login, amount := range roundRewards {
		total += amount
		tx.HIncrBy(ctx, r.formatKey("miners", login), "immature", amount)
		tx.HSetNX(ctx, r.formatKey("credits", "immature", block.Height, block.Hash), login, strconv.FormatInt(amount, 10))
	}
	tx.HIncrBy(ctx, r.formatKey("finances"), "immature", total)

	_, err := tx.Exec(ctx)
	return err
}

func (r *RedisClient) WriteMaturedBlock(block *BlockData, roundRewards map[string]int64) error {
	creditKey := r.formatKey("credits", "immature", block.RoundHeight, block.Hash)
	// tx, err := r.client.Watch(ctx, creditKey)
	// Must decrement immatures using existing log entry
	txf := func(tx *redis.Tx) error {
		immatureCredits := tx.HGetAll(ctx, creditKey)
		// if err != nil {
		// 	return err
		// }
		// defer tx.Close()

		ts := util.MakeTimestamp() / 1000
		value := join(block.Hash, ts, block.Reward)

		// _, err = tx.Exec(func() error {
		_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			r.writeMaturedBlock(tx, block)
			tx.ZAdd(ctx, r.formatKey("credits", "all"), redis.Z{Score: float64(block.Height), Member: value})

			// Decrement immature balances
			totalImmature := int64(0)
			for login, amountString := range immatureCredits.Val() {
				amount, _ := strconv.ParseInt(amountString, 10, 64)
				totalImmature += amount
				tx.HIncrBy(ctx, r.formatKey("miners", login), "immature", (amount * -1))
			}

			// Increment balances
			total := int64(0)
			for login, amount := range roundRewards {
				total += amount
				// NOTICE: Maybe expire round reward entry in 604800 (a week)?
				tx.HIncrBy(ctx, r.formatKey("miners", login), "balance", amount)
				tx.HSetNX(ctx, r.formatKey("credits", block.Height, block.Hash), login, strconv.FormatInt(amount, 10))
			}
			tx.Del(ctx, creditKey)
			tx.HIncrBy(ctx, r.formatKey("finances"), "balance", total)
			tx.HIncrBy(ctx, r.formatKey("finances"), "immature", (totalImmature * -1))
			tx.HSet(ctx, r.formatKey("finances"), "lastCreditHeight", strconv.FormatInt(block.Height, 10))
			tx.HSet(ctx, r.formatKey("finances"), "lastCreditHash", block.Hash)
			tx.HIncrBy(ctx, r.formatKey("finances"), "totalMined", block.RewardInSatoshi())
			return nil
		})
		return err
	}
	err := r.client.Watch(ctx, txf, creditKey)
	return err
}

func (r *RedisClient) WriteOrphan(block *BlockData) error {
	creditKey := r.formatKey("credits", "immature", block.RoundHeight, block.Hash)
	// tx, err := r.client.Watch(ctx, creditKey)
	// Must decrement immatures using existing log entry
	txf := func(tx *redis.Tx) error {
		immatureCredits := tx.HGetAll(ctx, creditKey)
		// if err != nil {
		// 	return err
		// }
		// defer tx.Close()

		// _, err = tx.Exec(func() error {
		_, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			r.writeMaturedBlock(tx, block)

			// Decrement immature balances
			totalImmature := int64(0)
			for login, amountString := range immatureCredits.Val() {
				amount, _ := strconv.ParseInt(amountString, 10, 64)
				totalImmature += amount
				tx.HIncrBy(ctx, r.formatKey("miners", login), "immature", (amount * -1))
			}
			tx.Del(ctx, creditKey)
			tx.HIncrBy(ctx, r.formatKey("finances"), "immature", (totalImmature * -1))
			return nil
		})
		return err
	}

	err := r.client.Watch(ctx, txf, creditKey)
	return err
}

func (r *RedisClient) WritePendingOrphans(blocks []*BlockData) error {
	tx := r.client.TxPipeline()
	// defer tx.Close()
	for _, block := range blocks {
		r.writeImmatureBlock(tx, block)
	}

	_, err := tx.Exec(ctx)
	return err
}

func (r *RedisClient) writeImmatureBlock(tx redis.Pipeliner, block *BlockData) {
	// Redis 2.8.x returns "ERR source and destination objects are the same"
	if block.Height != block.RoundHeight {
		tx.Rename(ctx, r.formatRound(block.RoundHeight, block.Nonce), r.formatRound(block.Height, block.Nonce))
	}
	tx.ZRem(ctx, r.formatKey("blocks", "candidates"), block.candidateKey)
	tx.ZAdd(ctx, r.formatKey("blocks", "immature"), redis.Z{Score: float64(block.Height), Member: block.key()})
}

func (r *RedisClient) writeMaturedBlock(tx *redis.Tx, block *BlockData) {
	tx.Del(ctx, r.formatRound(block.RoundHeight, block.Nonce))
	tx.ZRem(ctx, r.formatKey("blocks", "immature"), block.immatureKey)
	tx.ZAdd(ctx, r.formatKey("blocks", "matured"), redis.Z{Score: float64(block.Height), Member: block.key()})
}

func (r *RedisClient) IsMinerExists(login string) (int64, error) {
	return r.client.Exists(ctx, r.formatKey("miners", login)).Result()
}

func (r *RedisClient) GetMinerStats(login string, maxPayments int64) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	tx := r.client.TxPipeline()
	// defer tx.Close()
	tx.HGetAll(ctx, r.formatKey("miners", login))
	tx.ZRevRangeWithScores(ctx, r.formatKey("payments", login), 0, maxPayments-1)
	tx.ZCard(ctx, r.formatKey("payments", login))
	tx.HGet(ctx, r.formatKey("shares", "roundCurrent"), login)

	cmds, err := tx.Exec(ctx)

	if err != nil && err != redis.Nil {
		return nil, err
	} else {
		result, _ := cmds[0].(*redis.MapStringStringCmd).Result()
		stats["stats"] = convertStringMap(result)
		payments := convertPaymentsResults(cmds[1].(*redis.ZSliceCmd))
		stats["payments"] = payments
		stats["paymentsTotal"] = cmds[2].(*redis.IntCmd).Val()
		roundShares, _ := cmds[3].(*redis.StringCmd).Int64()
		stats["roundShares"] = roundShares
	}

	return stats, nil
}

// Try to convert all numeric strings to int64
func convertStringMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{})
	var err error
	for k, v := range m {
		result[k], err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			result[k] = v
		}
	}
	return result
}

// WARNING: Must run it periodically to flush out of window hashrate entries
func (r *RedisClient) FlushStaleStats(window, largeWindow time.Duration) (int64, error) {
	now := util.MakeTimestamp() / 1000
	max := fmt.Sprint("(", now-int64(window/time.Second))
	total, err := r.client.ZRemRangeByScore(ctx, r.formatKey("hashrate"), "-inf", max).Result()
	if err != nil {
		return total, err
	}

	n, err := r.client.ZRemRangeByScore(ctx, r.formatKey("invalidhashrate"), "-inf", max).Result()
	if err != nil {
		return total, err
	}
	total += n

	n, err = r.client.ZRemRangeByScore(ctx, r.formatKey("rejecthashrate"), "-inf", max).Result()
	if err != nil {
		return total, err
	}
	total += n

	var c uint64
	miners := make(map[string]struct{})
	max = fmt.Sprint("(", now-int64(largeWindow/time.Second))

	for {
		var keys []string
		var err error
		keys, c, err = r.client.Scan(ctx, c, r.formatKey("hashrate", "*"), 100).Result()
		if err != nil {
			return total, err
		}
		for _, row := range keys {
			login := strings.Split(row, ":")[2]
			if _, ok := miners[login]; !ok {
				n, err := r.client.ZRemRangeByScore(ctx, r.formatKey("hashrate", login), "-inf", max).Result()
				if err != nil {
					return total, err
				}
				miners[login] = struct{}{}
				total += n
			}
		}
		if c == 0 {
			break
		}
	}

	return total, nil
}

func (r *RedisClient) CollectStats(smallWindow time.Duration, maxBlocks, maxPayments int64) (map[string]interface{}, error) {
	window := int64(smallWindow / time.Second)
	stats := make(map[string]interface{})

	tx := r.client.TxPipeline()
	// defer tx.Close()

	now := util.MakeTimestamp() / 1000

	tx.ZRemRangeByScore(ctx, r.formatKey("hashrate"), "-inf", fmt.Sprint("(", now-window))
	tx.ZRangeWithScores(ctx, r.formatKey("hashrate"), 0, -1)
	tx.HGetAll(ctx, r.formatKey("stats"))
	tx.ZRevRangeWithScores(ctx, r.formatKey("blocks", "candidates"), 0, -1)
	tx.ZRevRangeWithScores(ctx, r.formatKey("blocks", "immature"), 0, -1)
	tx.ZRevRangeWithScores(ctx, r.formatKey("blocks", "matured"), 0, maxBlocks-1)
	tx.ZCard(ctx, r.formatKey("blocks", "candidates"))
	tx.ZCard(ctx, r.formatKey("blocks", "immature"))
	tx.ZCard(ctx, r.formatKey("blocks", "matured"))
	tx.ZCard(ctx, r.formatKey("payments", "all"))
	tx.ZRevRangeWithScores(ctx, r.formatKey("payments", "all"), 0, maxPayments-1)

	cmds, err := tx.Exec(ctx)

	if err != nil {
		return nil, err
	}

	result, _ := cmds[2].(*redis.MapStringStringCmd).Result()
	stats["stats"] = convertStringMap(result)
	candidates := convertCandidateResults(cmds[3].(*redis.ZSliceCmd))
	stats["candidates"] = candidates
	stats["candidatesTotal"] = cmds[6].(*redis.IntCmd).Val()

	immature := convertBlockResults(cmds[4].(*redis.ZSliceCmd))
	stats["immature"] = immature
	stats["immatureTotal"] = cmds[7].(*redis.IntCmd).Val()

	matured := convertBlockResults(cmds[5].(*redis.ZSliceCmd))
	stats["matured"] = matured
	stats["maturedTotal"] = cmds[8].(*redis.IntCmd).Val()

	payments := convertPaymentsResults(cmds[10].(*redis.ZSliceCmd))
	stats["payments"] = payments
	stats["paymentsTotal"] = cmds[9].(*redis.IntCmd).Val()

	totalHashrate, miners := convertMinersStats(window, cmds[1].(*redis.ZSliceCmd))
	stats["miners"] = miners
	stats["minersTotal"] = len(miners)
	stats["hashrate"] = totalHashrate
	return stats, nil
}

func (r *RedisClient) CollectWorkersStats(sWindow, lWindow time.Duration, login string) (map[string]interface{}, error) {
	smallWindow := int64(sWindow / time.Second)
	largeWindow := int64(lWindow / time.Second)
	stats := make(map[string]interface{})

	tx := r.client.TxPipeline()
	// defer tx.Close()

	now := util.MakeTimestamp() / 1000

	tx.ZRemRangeByScore(ctx, r.formatKey("hashrate", login), "-inf", fmt.Sprint("(", now-largeWindow))
	tx.ZRangeWithScores(ctx, r.formatKey("hashrate", login), 0, -1)

	cmds, err := tx.Exec(ctx)

	if err != nil {
		return nil, err
	}

	totalHashrate := int64(0)
	currentHashrate := int64(0)
	online := int64(0)
	offline := int64(0)
	workers := convertWorkersStats(smallWindow, cmds[1].(*redis.ZSliceCmd))

	for id, worker := range workers {
		timeOnline := now - worker.startedAt
		if timeOnline < 600 {
			timeOnline = 600
		}

		boundary := timeOnline
		if timeOnline >= smallWindow {
			boundary = smallWindow
		}
		worker.HR = worker.HR / boundary

		boundary = timeOnline
		if timeOnline >= largeWindow {
			boundary = largeWindow
		}
		worker.TotalHR = worker.TotalHR / boundary

		if worker.LastBeat < (now - smallWindow/2) {
			worker.Offline = true
			offline++
		} else {
			online++
		}

		currentHashrate += worker.HR
		totalHashrate += worker.TotalHR
		workers[id] = worker
	}
	stats["workers"] = workers
	stats["workersTotal"] = len(workers)
	stats["workersOnline"] = online
	stats["workersOffline"] = offline
	stats["hashrate"] = totalHashrate
	stats["currentHashrate"] = currentHashrate
	return stats, nil
}

func (r *RedisClient) CollectLuckStats(windows []int) (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	tx := r.client.TxPipeline()
	// defer tx.Close()

	max := int64(windows[len(windows)-1])

	tx.ZRevRangeWithScores(ctx, r.formatKey("blocks", "immature"), 0, -1)
	tx.ZRevRangeWithScores(ctx, r.formatKey("blocks", "matured"), 0, max-1)

	cmds, err := tx.Exec(ctx)
	if err != nil {
		return stats, err
	}
	blocks := convertBlockResults(cmds[0].(*redis.ZSliceCmd), cmds[1].(*redis.ZSliceCmd))

	calcLuck := func(max int) (int, float64, float64, float64) {
		var total int
		var sharesDiff, uncles, orphans float64
		for i, block := range blocks {
			if i > (max - 1) {
				break
			}
			if block.Uncle {
				uncles++
			}
			if block.Orphan {
				orphans++
			}
			sharesDiff += float64(block.TotalShares) / float64(block.Difficulty)
			total++
		}
		if total > 0 {
			sharesDiff /= float64(total)
			uncles /= float64(total)
			orphans /= float64(total)
		}
		return total, sharesDiff, uncles, orphans
	}
	for _, max := range windows {
		total, sharesDiff, uncleRate, orphanRate := calcLuck(max)
		row := map[string]float64{
			"luck": sharesDiff, "uncleRate": uncleRate, "orphanRate": orphanRate,
		}
		stats[strconv.Itoa(total)] = row
		if total < max {
			break
		}
	}
	return stats, nil
}

func convertCandidateResults(raw *redis.ZSliceCmd) []*BlockData {
	var result []*BlockData
	for _, v := range raw.Val() {
		// "nonce:eNonce1:eNonce2:timestamp:diff:totalShares:coinBaseValue:blkTotalFee"
		block := BlockData{}
		block.Height = int64(v.Score)
		block.RoundHeight = block.Height
		fields := strings.Split(v.Member.(string), ":")
		block.Nonce = fields[0]
		block.ENonce1 = fields[1]
		block.ENonce2 = fields[2]
		block.Timestamp, _ = strconv.ParseInt(fields[3], 10, 64)
		block.Difficulty, _ = strconv.ParseInt(fields[4], 10, 64)
		block.TotalShares, _ = strconv.ParseInt(fields[5], 10, 64)
		coinBaseValue, _ := strconv.ParseInt(fields[6], 10, 64)
		block.CoinBaseValue = big.NewInt(coinBaseValue)
		blkTotalFee, _ := strconv.ParseInt(fields[7], 10, 64)
		block.BlkTotalFee = big.NewInt(blkTotalFee)
		block.candidateKey = v.Member.(string)
		result = append(result, &block)
	}
	return result
}

func convertBlockResults(rows ...*redis.ZSliceCmd) []*BlockData {
	var result []*BlockData
	for _, row := range rows {
		for _, v := range row.Val() {
			// "uncleHeight:orphan:nonce:blockHash:timestamp:diff:totalShares:coinBaseValue:blkTotalFee:rewardInSatoshi"
			block := BlockData{}
			block.Height = int64(v.Score)
			block.RoundHeight = block.Height
			fields := strings.Split(v.Member.(string), ":")
			block.UncleHeight = 0
			block.Uncle = false
			block.Orphan, _ = strconv.ParseBool(fields[1])
			block.Nonce = fields[2]
			block.Hash = fields[3]
			block.Timestamp, _ = strconv.ParseInt(fields[4], 10, 64)
			block.Difficulty, _ = strconv.ParseInt(fields[5], 10, 64)
			block.TotalShares, _ = strconv.ParseInt(fields[6], 10, 64)

			block.CoinBaseValue, _ = new(big.Int).SetString(fields[7], 10)
			block.BlkTotalFee, _ = new(big.Int).SetString(fields[8], 10)

			block.RewardString = fields[9]
			block.ImmatureReward = fields[9]
			block.immatureKey = v.Member.(string)
			result = append(result, &block)
		}
	}
	return result
}

// Build per login workers's total shares map {'rig-1': 12345, 'rig-2': 6789, ...}
// TS => diff, id, ms
func convertWorkersStats(window int64, raw *redis.ZSliceCmd) map[string]Worker {
	now := util.MakeTimestamp() / 1000
	workers := make(map[string]Worker)

	for _, v := range raw.Val() {
		parts := strings.Split(v.Member.(string), ":")
		share, _ := strconv.ParseInt(parts[0], 10, 64)
		id := parts[1]
		score := int64(v.Score)
		worker := workers[id]

		// Add for large window
		worker.TotalHR += share

		// Add for small window if matches
		if score >= now-window {
			worker.HR += share
		}

		if worker.LastBeat < score {
			worker.LastBeat = score
		}
		if worker.startedAt > score || worker.startedAt == 0 {
			worker.startedAt = score
		}
		workers[id] = worker
	}
	return workers
}

func convertMinersStats(window int64, raw *redis.ZSliceCmd) (int64, map[string]Miner) {
	now := util.MakeTimestamp() / 1000
	miners := make(map[string]Miner)
	totalHashrate := int64(0)

	for _, v := range raw.Val() {
		parts := strings.Split(v.Member.(string), ":")
		share, _ := strconv.ParseInt(parts[0], 10, 64)
		id := parts[1]
		score := int64(v.Score)
		miner := miners[id]
		miner.HR += share

		if miner.LastBeat < score {
			miner.LastBeat = score
		}
		if miner.startedAt > score || miner.startedAt == 0 {
			miner.startedAt = score
		}
		miners[id] = miner
	}

	for id, miner := range miners {
		timeOnline := now - miner.startedAt
		if timeOnline < 600 {
			timeOnline = 600
		}

		boundary := timeOnline
		if timeOnline >= window {
			boundary = window
		}
		miner.HR = miner.HR / boundary

		if miner.LastBeat < (now - window/2) {
			miner.Offline = true
		}
		totalHashrate += miner.HR
		miners[id] = miner
	}
	return totalHashrate, miners
}

func convertPaymentsResults(raw *redis.ZSliceCmd) []map[string]interface{} {
	var result []map[string]interface{}
	for _, v := range raw.Val() {
		tx := make(map[string]interface{})
		tx["timestamp"] = int64(v.Score)
		fields := strings.Split(v.Member.(string), ":")
		tx["tx"] = fields[0]
		// Individual or whole payments row
		if len(fields) < 3 {
			tx["amount"], _ = strconv.ParseInt(fields[1], 10, 64)
		} else {
			tx["address"] = fields[1]
			tx["amount"], _ = strconv.ParseInt(fields[2], 10, 64)
		}
		result = append(result, tx)
	}
	return result
}
