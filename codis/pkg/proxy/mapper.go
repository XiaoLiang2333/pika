// Copyright 2016 CodisLabs. All Rights Reserved.
// Licensed under the MIT (MIT-LICENSE.txt) license.

package proxy

import (
	"bytes"
	"hash/crc32"
	"strconv"
	"strings"
	"sync"

	"pika/codis/v2/pkg/proxy/redis"
	"pika/codis/v2/pkg/utils/errors"
	"pika/codis/v2/pkg/utils/log"
)

var charmap [256]byte

func init() {
	for i := range charmap {
		c := byte(i)
		switch {
		case c >= 'A' && c <= 'Z':
			charmap[i] = c
		case c >= 'a' && c <= 'z':
			charmap[i] = c - 'a' + 'A'
		case c == ':':
			charmap[i] = ':'
		}
	}
}

type OpFlag uint32
type OpFlagChecker uint32
func (f OpFlag) IsNotAllowed() bool {
	return (f & FlagNotAllow) != 0
}

func (f OpFlag) IsReadOnly() bool {
	const mask = FlagWrite | FlagMayWrite
	return (f & mask) == 0
}

func (f OpFlag) IsMasterOnly() bool {
	const mask = FlagWrite | FlagMayWrite | FlagMasterOnly
	return (f & mask) != 0
}

func (f OpFlag) IsQuick() bool {
	return (f & FlagQuick) != 0
}

type OpInfo struct {
	Name string
	Flag OpFlag
	FlagMonitor OpFlagChecker
	CustomCheckFunc
}

const (
	FlagWrite OpFlag = 1 << iota
	FlagMasterOnly
	FlagMayWrite
	FlagNotAllow
	FlagQuick
	FlagSlow
)
const (
	// -- Request Part
	// 1. The request contains multiple keys or multiple members and has no value, that is, the size of the data does not need to be considered
	FlagReqKeys      = 1 << iota // 1     Arguments：CMD Key1 ~ KeyN
	FlagReqKeyFields             // 2     Arguments：CMD KEY Field1 ~ FieldN

	// 2. The request not only contains keys or members, but also manipulates the value of the response and needs to check the amount of data
	FlagReqValues         // 4     Arguments：CMD KEY Value1 ~ ValueN
	FlagReqKeyValues      // 8     Arguments：CMD (Key1,Value1) ~ (KeyN,ValueN)
	FlagReqKeyFieldValues // 16    Arguments：CMD KEY (Field1,Value1) ~ (FieldN,ValueN)
	FlagReqKeyTtlValue    // 32    Arguments：CMD KEY ttl value

	// -- Response Part
	// 1. The response returns a single number representing either the number of members of the key or the size of the key itself (if key is of type string)
	FlagRespReturnArraysize // 64    The return value is the length of the array
	FlagRespReturnValuesize //	128    The returned value is the length of a string

	// 2. The response returns the result of the query, representing the key itself or part of it
	FlagRespReturnSingleValue // 256   The return value is a single value
	FlagRespReturnArray       // 512   Return as an array, one as a group
	FlagRespReturnArrayByPair // 1024   Return as an array, two as a group

	// 3. The response returns an array, but only the size of the array is checked, not the contents of the array
	FlagRespCheckArrayLength       // 2048  Returns an array, one as a group, checking only the length of the array, not the contents of the array
	FlagRespCheckArrayLengthByPair // 4096  Returns an array, two in a group, checking only the length of the array, not the contents of the array
)

// Flat bit：BigKey，BigValue judgment
func (f OpFlagChecker) NeedCheckBatchsizeOfRequest() bool {
	const mask = FlagReqKeys | FlagReqKeyFields
	return (f & mask) != 0
}

func (f OpFlagChecker) NeedCheckContentOfRequest() bool {
	const mask = FlagReqValues | FlagReqKeyValues | FlagReqKeyFieldValues | FlagReqKeyTtlValue
	return (f & mask) != 0
}

func (f OpFlagChecker) NeedCheckSingleValueOfResp() bool {
	const mask = FlagRespReturnSingleValue
	return (f & mask) != 0
}

func (f OpFlagChecker) NeedCheckNumberOfResp() bool {
	const mask = FlagRespReturnArraysize | FlagRespReturnValuesize
	return (f & mask) != 0
}

func (f OpFlagChecker) NeedCheckArrayOfResp() bool {
	const mask = FlagRespReturnArray | FlagRespReturnArrayByPair | FlagRespCheckArrayLength | FlagRespCheckArrayLengthByPair
	return (f & mask) != 0
}

type CustomCheckFunc interface {
	CheckRequest(r *Request) bool  //return true indicates that it has been checked
	CheckResponse(r *Request) bool //return true indicates that it has been checked
}

var (
	opTableLock sync.RWMutex
	opTable     = make(map[string]OpInfo, 256)
)

func init() {
	for _, i := range []OpInfo{
		{"APPEND", FlagWrite, FlagReqKeyValues | FlagRespReturnValuesize, nil},
		{"ASKING", FlagNotAllow, 0, nil},
		{"AUTH", 0, 0, nil},
		{"BGREWRITEAOF", FlagNotAllow, 0, nil},
		{"BGSAVE", FlagNotAllow, 0, nil},
		{"BITCOUNT", 0, 0, nil},
		{"BITFIELD", FlagWrite, 0, nil},
		{"BITOP", FlagWrite | FlagNotAllow, 0, nil},
		{"BITPOS", 0, 0, nil},
		{"BLPOP", FlagWrite | FlagNotAllow, 0, nil},
		{"BRPOP", FlagWrite | FlagNotAllow, 0, nil},
		{"BRPOPLPUSH", FlagWrite | FlagNotAllow, 0, nil},
		{"CLIENT", FlagNotAllow, 0, nil},
		{"CLUSTER", FlagNotAllow, 0, nil},
		{"COMMAND", 0, 0, nil},
		{"CONFIG", FlagNotAllow, 0, nil},
		{"DBSIZE", FlagNotAllow, 0, nil},
		{"DEBUG", FlagNotAllow, 0, nil},
		{"DECR", FlagWrite, 0, nil},
		{"DECRBY", FlagWrite, 0, nil},
		{"DEL", FlagWrite, FlagReqKeys, nil},
		{"DISCARD", FlagNotAllow, 0, nil},
		{"DUMP", 0, 0, nil},
		{"ECHO", 0, 0, nil},
		{"EVAL", FlagNotAllow, 0, nil},
		{"EVALSHA", FlagNotAllow, 0, nil},
		{"EXEC", FlagNotAllow, 0, nil},
		{"EXISTS", 0, 0, nil},
		{"EXPIRE", FlagWrite, 0, nil},
		{"EXPIREAT", FlagWrite, 0, nil},
		{"FLUSHALL", FlagWrite | FlagNotAllow, 0, nil},
		{"FLUSHDB", FlagWrite | FlagNotAllow, 0, nil},
		{"GEOADD", FlagWrite, 0, nil},
		{"GEODIST", 0, 0, nil},
		{"GEOHASH", 0, 0, nil},
		{"GEOPOS", 0, 0, nil},
		{"GEORADIUS", FlagWrite, 0, nil},
		{"GEORADIUSBYMEMBER", FlagWrite, 0, nil},
		{"GET", 0, FlagRespReturnSingleValue, nil},
		{"GETBIT", 0, 0, nil},
		{"GETRANGE", 0, 0, nil},
		{"GETSET", FlagWrite, FlagReqKeyValues | FlagRespReturnSingleValue, nil},
		{"HDEL", FlagWrite, FlagReqKeyFields, nil},
		{"HEXISTS", 0, 0, nil},
		{"HGET", 0, 0, &CheckHGET{}},
		{"HGETALL", 0, FlagRespReturnArrayByPair, nil},
		{"HINCRBY", FlagWrite, 0, nil},
		{"HINCRBYFLOAT", FlagWrite, 0, nil},
		{"HKEYS", 0, FlagRespCheckArrayLength, nil},
		{"HLEN", 0, FlagRespReturnArraysize, nil},
		{"HMGET", 0, FlagReqKeyFields | FlagRespReturnArray, nil},
		{"HMSET", FlagWrite, FlagReqKeyFieldValues, nil},
		{"HOST:", FlagNotAllow, 0, nil},
		{"HSCAN", FlagMasterOnly, 0, nil},
		{"HSET", FlagWrite, FlagReqKeyFieldValues, nil},
		{"HSETNX", FlagWrite, FlagReqKeyFieldValues, nil},
		{"HSTRLEN", 0, 0, nil},
		{"HVALS", 0, 0, nil},
		{"INCR", FlagWrite, 0, nil},
		{"INCRBY", FlagWrite, 0, nil},
		{"INCRBYFLOAT", FlagWrite, 0, nil},
		{"INFO", 0, 0, nil},
		{"KEYS", FlagNotAllow, 0, nil},
		{"LASTSAVE", FlagNotAllow, 0, nil},
		{"LATENCY", FlagNotAllow, 0, nil},
		{"LINDEX", 0, 0, nil},
		{"LINSERT", FlagWrite, 0, nil},
		{"LLEN", 0, FlagRespReturnArraysize, nil},
		{"LPOP", FlagWrite, 0, nil},
		{"LPUSH", FlagWrite, FlagReqKeyFields | FlagRespReturnArraysize, nil},
		{"LPUSHX", FlagWrite, FlagReqKeyFields | FlagRespReturnArraysize, nil},
		{"LRANGE", 0, FlagRespCheckArrayLength, &CheckLRANGE{}},
		{"LREM", FlagWrite, FlagRespReturnArraysize, nil},
		{"LSET", FlagWrite, 0, nil},
		{"LTRIM", FlagWrite, 0, nil},
		{"MGET", 0, FlagReqKeys, &CheckMGET{}},
		{"MIGRATE", FlagWrite | FlagNotAllow, 0, nil},
		{"MONITOR", FlagNotAllow, 0, nil},
		{"MOVE", FlagWrite | FlagNotAllow, 0, nil},
		{"MSET", FlagWrite, FlagReqKeyValues, nil},
		{"MSETNX", FlagWrite | FlagNotAllow, FlagReqKeyValues, nil},
		{"MULTI", FlagNotAllow, 0, nil},
		{"OBJECT", FlagNotAllow, 0, nil},
		{"PERSIST", FlagWrite, 0, nil},
		{"PEXPIRE", FlagWrite, 0, nil},
		{"PEXPIREAT", FlagWrite, 0, nil},
		{"PFADD", FlagWrite, 0, nil},
		{"PFCOUNT", 0, 0, nil},
		{"PFDEBUG", FlagWrite, 0, nil},
		{"PFMERGE", FlagNotAllow, 0, nil},
		{"PFSELFTEST", 0, 0, nil},
		{"PING", 0, 0, nil},
		{"POST", FlagNotAllow, 0, nil},
		{"PSETEX", FlagWrite, FlagReqKeyTtlValue, nil},
		{"PSUBSCRIBE", FlagNotAllow, 0, nil},
		{"PSYNC", FlagNotAllow, 0, nil},
		{"PTTL", 0, 0, nil},
		{"PUBLISH", FlagNotAllow, 0, nil},
		{"PUBSUB", 0, 0, nil},
		{"PUNSUBSCRIBE", FlagNotAllow, 0, nil},
		{"QUIT", 0, 0, nil},
		{"RANDOMKEY", FlagNotAllow, 0, nil},
		{"READONLY", FlagNotAllow, 0, nil},
		{"READWRITE", FlagNotAllow, 0, nil},
		{"RENAME", FlagWrite | FlagNotAllow, 0, nil},
		{"RENAMENX", FlagWrite | FlagNotAllow, 0, nil},
		{"REPLCONF", FlagNotAllow, 0, nil},
		{"RESTORE", FlagWrite | FlagNotAllow, 0, nil},
		{"RESTORE-ASKING", FlagWrite | FlagNotAllow, 0, nil},
		{"ROLE", 0, 0, nil},
		{"RPOP", FlagWrite, 0, nil},
		{"RPOPLPUSH", FlagNotAllow, 0, nil},
		{"RPUSH", FlagWrite, FlagReqKeyFields | FlagRespReturnArraysize, nil},
		{"RPUSHX", FlagWrite, FlagReqKeyFields | FlagRespReturnArraysize, nil},
		{"SADD", FlagWrite, FlagReqKeyFields, nil},
		{"SAVE", FlagNotAllow, 0, nil},
		{"SCAN", FlagMasterOnly | FlagNotAllow, 0, nil},
		{"SCARD", 0, FlagRespReturnArraysize, nil},
		{"SCRIPT", FlagNotAllow, 0, nil},
		{"SDIFF", FlagNotAllow, FlagReqKeys, &CheckSDIFF{}},
		{"SDIFFSTORE", FlagWrite, 0, nil},
		{"SELECT", 0, 0, nil},
		{"SET", FlagWrite, 0, &CheckSET{}},
		{"SETBIT", FlagWrite, 0, nil},
		{"SETEX", FlagWrite, FlagReqKeyTtlValue, nil},
		{"SETNX", FlagWrite, FlagReqKeyValues, nil},
		{"SETRANGE", FlagWrite, FlagReqKeyFieldValues | FlagRespReturnValuesize, nil},
		{"SHUTDOWN", FlagNotAllow, 0, nil},
		{"SINTER", FlagNotAllow, FlagReqKeys, nil},
		{"SINTERSTORE", FlagNotAllow, FlagReqKeys, nil},
		{"SISMEMBER", 0, 0, nil},
		{"SLAVEOF", FlagNotAllow, 0, nil},
		{"SLOTSCHECK", FlagNotAllow, 0, nil},
		{"SLOTSDEL", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSHASHKEY", 0, 0, nil},
		{"SLOTSINFO", FlagMasterOnly, 0, nil},
		{"SLOTSMAPPING", 0, 0, nil},
		{"SLOTSMGRTONE", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRTSLOT", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRTTAGONE", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRTTAGSLOT", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSRESTORE", FlagWrite, 0, nil},
		{"SLOTSMGRTONE-ASYNC", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRTSLOT-ASYNC", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRTTAGONE-ASYNC", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRTTAGSLOT-ASYNC", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSMGRT-ASYNC-FENCE", FlagNotAllow, 0, nil},
		{"SLOTSMGRT-ASYNC-CANCEL", FlagNotAllow, 0, nil},
		{"SLOTSMGRT-ASYNC-STATUS", FlagNotAllow, 0, nil},
		{"SLOTSMGRT-EXEC-WRAPPER", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSRESTORE-ASYNC", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSRESTORE-ASYNC-AUTH", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSRESTORE-ASYNC-ACK", FlagWrite | FlagNotAllow, 0, nil},
		{"SLOTSSCAN", FlagMasterOnly, 0, nil},
		{"SLOWLOG", FlagNotAllow, 0, nil},
		{"SMEMBERS", 0, FlagRespCheckArrayLength, nil},
		{"SMOVE", FlagNotAllow, 0, nil},
		{"SORT", FlagWrite, 0, nil},
		{"SPOP", FlagWrite, FlagRespCheckArrayLength, &CheckSPOP{}},
		{"SRANDMEMBER", 0, FlagRespCheckArrayLength, &CheckSRANDMEMBER{}},
		{"SREM", FlagWrite, FlagReqKeyFields, nil},
		{"SSCAN", FlagMasterOnly, 0, nil},
		{"STRLEN", 0, FlagRespReturnValuesize, nil},
		{"SUBSCRIBE", FlagNotAllow, 0, nil},
		{"SUBSTR", 0, 0, nil},
		{"SUNION", FlagNotAllow, FlagReqKeys, nil},
		{"SUNIONSTORE", FlagNotAllow, FlagReqKeys, nil},
		{"SYNC", FlagNotAllow, 0, nil},
		{"PCONFIG", 0, 0, nil},
		{"TIME", FlagNotAllow, 0, nil},
		{"TOUCH", FlagWrite, 0, nil},
		{"TTL", 0, 0, nil},
		{"TYPE", 0, 0, nil},
		{"UNSUBSCRIBE", FlagNotAllow, 0, nil},
		{"UNWATCH", FlagNotAllow, 0, nil},
		{"WAIT", FlagNotAllow, 0, nil},
		{"WATCH", FlagNotAllow, 0, nil},
		{"XSLOWLOG", 0, 0, nil},
		{"XMONITOR", 0, 0, nil},
		{"XCONFIG", 0, 0, nil},
		{"ZADD", FlagWrite, 0, nil},
		{"ZCARD", 0, FlagRespReturnArraysize, nil},
		{"ZCOUNT", 0, 0, nil},
		{"ZINCRBY", FlagWrite, 0, nil},
		{"ZINTERSTORE", FlagNotAllow, 0, nil},
		{"ZLEXCOUNT", 0, 0, nil},
		{"ZRANGE", 0, 0, &CheckZRANGE{}},
		{"ZRANGEBYLEX", 0, 0, nil},
		{"ZRANGEBYSCORE", 0, 0, nil},
		{"ZRANK", 0, 0, nil},
		{"ZREM", FlagWrite, FlagReqKeyFields, nil},
		{"ZREMRANGEBYLEX", FlagWrite, 0, nil},
		{"ZREMRANGEBYRANK", FlagWrite, 0, nil},
		{"ZREMRANGEBYSCORE", FlagWrite, 0, nil},
		{"ZREVRANGE", 0, 0, nil},
		{"ZREVRANGEBYLEX", 0, 0, nil},
		{"ZREVRANGEBYSCORE", 0, 0, nil},
		{"ZREVRANK", 0, 0, nil},
		{"ZSCAN", FlagMasterOnly, 0, nil},
		{"ZSCORE", 0, 0, nil},
		{"ZUNIONSTORE", FlagNotAllow, 0, nil},
	} {
		opTable[i.Name] = i
	}
}


var (
	ErrBadMultiBulk = errors.New("bad multi-bulk for command")
	ErrBadOpStrLen  = errors.New("bad command length, too short or too long")
)

const MaxOpStrLen = 64

func getOpInfo(multi []*redis.Resp) (string, OpFlag, OpFlagChecker, CustomCheckFunc, error) {
	if len(multi) < 1 {
		return "", 0, 0, nil, ErrBadMultiBulk
	}

	var upper [MaxOpStrLen]byte

	var op = multi[0].Value
	if len(op) == 0 || len(op) > len(upper) {
		return "", 0, 0, nil, ErrBadOpStrLen
	}
	for i := range op {
		if c := charmap[op[i]]; c != 0 {
			upper[i] = c
		} else {
			return strings.ToUpper(string(op)), FlagMayWrite, 0, nil, nil
		}
	}
	op = upper[:len(op)]

	opTableLock.RLock()
	defer opTableLock.RUnlock()

	if r, ok := opTable[string(op)]; ok {
		return r.Name, r.Flag, r.FlagMonitor, r.CustomCheckFunc, nil
	}
	return string(op), FlagMayWrite, 0, nil, nil
}

func Hash(key []byte) uint32 {
	const (
		TagBeg = '{'
		TagEnd = '}'
	)
	if beg := bytes.IndexByte(key, TagBeg); beg >= 0 {
		if end := bytes.IndexByte(key[beg+1:], TagEnd); end >= 0 {
			key = key[beg+1 : beg+1+end]
		}
	}
	return crc32.ChecksumIEEE(key)
}

func getHashKey(multi []*redis.Resp, opstr string) []byte {
	var index = 1
	switch opstr {
	case "ZINTERSTORE", "ZUNIONSTORE", "EVAL", "EVALSHA":
		index = 3
	}
	if index < len(multi) {
		return multi[index].Value
	}
	return nil
}

func getWholeCmd(multi []*redis.Resp, cmd []byte) int {
	var (
		index = 0
		bytes = 0
	)
	for i := 0; i < len(multi); i++ {
		if index < len(cmd) {
			index += copy(cmd[index:], multi[i].Value)
			if i < len(multi)-i {
				index += copy(cmd[index:], []byte(" "))
			}
		}
		bytes += len(multi[i].Value)

		if i == len(multi)-1 && index == len(cmd) {
			more := []byte("... " + strconv.Itoa(len(multi)) + " elements " + strconv.Itoa(bytes) + " bytes.")
			index = len(cmd) - len(more)
			if index < 0 {
				index = 0
			}
			index += copy(cmd[index:], more)
			break
		}
	}
	return index
}

func setCmdListFlag(cmdlist string, flag OpFlag) error {
	reverseFlag := FlagSlow
	flagString := "FlagQuick"
	if flag&FlagSlow != 0 {
		reverseFlag = FlagQuick
		flagString = "FlagSlow"
	}

	opTableLock.Lock()
	defer opTableLock.Unlock()

	for _, r := range opTable {
		r.Flag = r.Flag &^ flag
		opTable[r.Name] = r
	}
	if len(cmdlist) == 0 {
		return nil
	}
	cmdlist = strings.ToUpper(cmdlist)
	cmds := strings.Split(cmdlist, ",")
	for i := 0; i < len(cmds); i++ {
		if r, ok := opTable[strings.TrimSpace(cmds[i])]; ok {
			log.Infof("before setCmdListFlag: r.Name[%s], r.Flag[%d]", r.Name, r.Flag)
			if r.Flag&reverseFlag == 0 {
				r.Flag = r.Flag | flag
				opTable[strings.TrimSpace(cmds[i])] = r
				log.Infof("after setCmdListFlag: r.Name[%s], r.Flag[%d]", r.Name, r.Flag)
			} else {
				log.Warnf("cmd[%s] is %s command.", cmds[i], flagString)
				return errors.Errorf("cmd[%s] is %s command.", cmds[i], flagString)
			}
		} else {
			log.Warnf("can not find [%s] command.", cmds[i])
			return errors.Errorf("can not find [%s] command.", cmds[i])
		}
	}
	return nil
}

func getCmdFlag() *redis.Resp {
	var array = make([]*redis.Resp, 0, 32)
	const mask = FlagQuick | FlagSlow

	opTableLock.RLock()
	defer opTableLock.RUnlock()

	for _, r := range opTable {
		if r.Flag&mask != 0 {
			retStr := r.Name + " : Flag[" + strconv.Itoa(int(r.Flag)) + "]"

			if r.Flag&FlagQuick != 0 {
				retStr += ", FlagQuick"
			}

			if r.Flag&FlagSlow != 0 {
				retStr += ", FlagSlow"
			}

			array = append(array, redis.NewBulkBytes([]byte(retStr)))
		}
	}
	return redis.NewArray(array)
}
