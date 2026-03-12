-- mlog_insert.lua — sysbench custom script for mlog write overhead benchmark
-- Each event() executes one multi-row INSERT into the base table.

sysbench.cmdline.options = {
  batch_size = {"Rows per INSERT statement", 1},
  txn_mode   = {"Transaction mode: optimistic or pessimistic", "optimistic"},
  table_name = {"Target table name", "bc_bet_records"},
}

local drv
local con
local tid
local counter

function thread_init()
  drv = sysbench.sql.driver()
  con = drv:connect()

  con:query("SET SESSION tidb_txn_mode='" .. sysbench.opt.txn_mode .. "'")
  con:query("SET SESSION tidb_dml_type='standard'")

  tid = sysbench.tid + 1  -- 1-based thread id
  counter = 0
end

-- Data distribution helpers

local function rand_account()
  return string.format("user_%07d", sysbench.rand.uniform(1, 1000000))
end

local function rand_site_code()
  return string.format("SITE_%02d", sysbench.rand.uniform(1, 50))
end

local function rand_platform()
  return sysbench.rand.uniform(1, 20)
end

local function rand_category()
  return sysbench.rand.uniform(1, 20)
end

local function rand_game_id()
  return sysbench.rand.uniform(1, 2000)
end

local function rand_currency()
  local currencies = {"CNY", "USD", "EUR", "GBP", "JPY"}
  return currencies[sysbench.rand.uniform(1, 5)]
end

local function rand_settle_status()
  local r = sysbench.rand.uniform(1, 100)
  if r <= 80 then return 2      -- settled 80%
  elseif r <= 99 then return 1  -- unsettled 19%
  else return 3 end             -- cancelled 1%
end

local function rand_decimal(max)
  return sysbench.rand.uniform(0, max * 100) / 100.0
end

local function rand_datetime_90d()
  local now = os.time()
  local offset = sysbench.rand.uniform(0, 90 * 86400)
  return os.date("%Y-%m-%d %H:%M:%S", now - offset)
end

function event()
  local batch = sysbench.opt.batch_size
  local cols = "id, record_id, order_no, round_id, platform_id, category_id, " ..
               "site_code, site_prefix, agent_code, account, third_user_name, " ..
               "pull_time, third_game_code, all_bet, valid_bet, net_profit, " ..
               "rake, jackpot, bet_time, bet_time_stamp, settle_time, " ..
               "settle_time_stamp, settle_status, device, bet_ip, " ..
               "third_group_code, after_balance, is_combo, odds_type, odds, " ..
               "order_status, sports_type, winlost_time, game_id, currency, " ..
               "settle_time_zone, settle_date, version_no, tax_rate, tax"

  local values_list = {}
  for i = 1, batch do
    counter = counter + 1
    local id = tid * 4294967296 + counter  -- tid << 32
    local account = rand_account()
    local site_code = rand_site_code()
    local platform_id = rand_platform()
    local category_id = rand_category()
    local game_id = rand_game_id()
    local currency = rand_currency()
    local settle_status = rand_settle_status()
    local settle_tz = rand_datetime_90d()
    local settle_date = string.sub(settle_tz, 1, 10)
    local bet_time = rand_datetime_90d()
    local now_ts = os.time()

    local vals = string.format(
      "(%d, 'REC-%d-%d', 'ORD-%d-%d', 'RND-%d-%d', %d, %d, " ..
      "'%s', 'PRE_%s', 'AGT_%s', '%s', 'TU_%s', " ..
      "NOW(), 'GC_%d', %.2f, %.2f, %.2f, " ..
      "%.2f, %.2f, '%s', %d, '%s', " ..
      "%d, %d, 'mobile', '10.0.%d.%d', " ..
      "'GRP_%d', %.2f, %d, 'EU', %.2f, " ..
      "'active', 'football', NOW(), %d, '%s', " ..
      "'%s', '%s', 0, %.4f, %.4f)",
      id, tid, counter, tid, counter, tid, counter,
      platform_id, category_id,
      site_code, site_code, site_code, account, account,
      game_id,
      rand_decimal(10000), rand_decimal(10000), rand_decimal(5000),
      rand_decimal(1000), rand_decimal(5000),
      bet_time, now_ts, settle_tz,
      now_ts, settle_status,
      sysbench.rand.uniform(0, 255), sysbench.rand.uniform(0, 255),
      sysbench.rand.uniform(1, 100),
      rand_decimal(50000), sysbench.rand.uniform(0, 1), rand_decimal(10),
      game_id, currency,
      settle_tz, settle_date,
      rand_decimal(0.1), rand_decimal(100)
    )
    values_list[i] = vals
  end

  local sql = "INSERT INTO " .. sysbench.opt.table_name ..
              " (" .. cols .. ") VALUES " .. table.concat(values_list, ", ")
  con:query(sql)
end

function thread_done()
  con:disconnect()
end
