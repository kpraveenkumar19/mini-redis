<h1>
  <img src="assets/images/icon.png" alt="mini-redis icon" width="36" height="36" style="vertical-align: middle; margin-right: 8px;" />
  Mini Redis
</h1>

Minimal Redis-like server and CLI in Go. It supports a subset of [Redis](https://redis.io) command (keys with optional expiry, lists including blocking pops, sorted sets, reading from a RDB file) and speaks about the Redis Serialization Protocol -[RESP](https://redis.io/docs/latest/develop/reference/protocol-spec/) and [Redis Persistence](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/). It ships with a tiny `mini-redis-server` and a companion `mini-redis-cli` for interactive use.


## Table of Contents

- [Overview](#overview)
- [Features](#features)
- [Installation](#installation)
  - [macOS (Homebrew)](#macos-homebrew)
  - [Windows](#windows)
- [Usage](#usage)
  - [Starting the server](#starting-the-server)
  - [Starting the CLI](#starting-the-cli)
  - [Commands](#commands)
- [Resources](#resources)
- [Contributing](#contributing)


## Overview

This repository contains a implementation of a small, in-memory data store with a Redis-like protocol and command set. The server handles concurrent clients over TCP, parses RESP requests, and executes a core subset of [Redis commands](https://redis.io/docs/latest/commands//?group=bf) for strings, lists, and sorted sets. It can also best-effort load an existing RDB file at startup for simple persistence scenarios (read-only load).

The project is written in Go to demonstrate concurrency, networking, and protocol parsing with a familiar Redis-like interface.


## Features

- **RESP protocol**: Parses and returns replies using Redis Serialization Protocol (RESP).

- **Keys with optional Expiry**:
  
  - `SET key value [PX <milliseconds>]`
  - `GET key`
  - Passive expiration on read when PX is set.
 
- **Lists**:
  
  - `LPUSH`, `RPUSH`, `LLEN`, `LRANGE`, `LPOP [count]`, `BLPOP key timeout`
  - Basic blocking pop support with timeout or indefinite block.

- **Sorted Sets (ZSET)**:

  - `ZADD`, `ZRANK`, `ZRANGE`, `ZCARD`, `ZSCORE`, `ZREM`
  - Kept ordered by score (and member name as tiebreaker).
 
- **Introspection and config**:

  - `PING`, `ECHO <message>`
  - `KEYS *` (only the `*` pattern is supported)
  - `CONFIG GET dir|dbfilename`
 
- **RDB loading (read-only)**:

  - If `-dir` and `-dbfilename` are given, the server attempts to load an RDB file on startup.

- **Simple, portable CLI**:

  - `mini-redis-cli` provides an interactive prompt similar to `redis-cli`.


## Installation

### macOS (Homebrew)

```bash
brew tap kpraveenkumar19/mini-redis
brew install mini-redis
```

This installs both `mini-redis-server` and `mini-redis-cli` on your PATH.

### Windows

```bash
git clone https://github.com/kpraveenkumar19/mini-redis.git
cd mini-redis

# Build the server and cli
go build -o bin/mini-redis-server ./cmd/mini-redis-server
go build -o bin/mini-redis-cli ./cmd/mini-redis-cli

# Starting the server and cli
./bin/mini-redis-server
./bin/mini-redis-cli
```


## Usage

### Starting the server

```bash
mini-redis-server
```

### Starting the CLI

```bash
mini-redis-cli 
```

You will see an interactive prompt like:

```
mini-redis-cli> SET key boo
OK
mini-redis-cli> GET key
boo
```

Type `exit` or `quit` to leave the CLI.


### Commands

Below are all commands implemented by this project with example interactions :


#### Connection and introspection

```text
mini-redis-cli> PING
PONG

mini-redis-cli> ECHO hello
hello

mini-redis-cli> KEYS *
1) <returns keys read from an RDB file>

mini-redis-cli> CONFIG GET dir
1) dir
2) <directory containing the rdb file>
mini-redis-cli> CONFIG GET dbfilename
1) dbfilename
2) <rdb file name>
```

#### Strings

```text
# Basic set/get
mini-redis-cli> SET greeting "hello"
OK
mini-redis-cli> GET greeting
hello

# Set with millisecond (PX)
mini-redis-cli> SET temp "42" PX 2000
OK
mini-redis-cli> GET temp
42
# After ~2 seconds, the key will expire lazily on read
```

#### Lists

```text
# Push items
mini-redis-cli> LPUSH mylist a b c    # list is now [c b a]
(integer) 3
mini-redis-cli> RPUSH mylist d e      # list is now [c b a d e]
(integer) 5

# Length
mini-redis-cli> LLEN mylist
(integer) 5

# Range (supports negative indexes)
mini-redis-cli> LRANGE mylist 1 3
1) b
2) a
3) d

# Pop single or multiple from head
mini-redis-cli> LPOP mylist
c
mini-redis-cli> LPOP mylist 2
1) b
2) a

# Blocking pop with timeout (seconds; 0 = block indefinitely)
mini-redis-cli> BLPOP emptylist 2
(nil)
mini-redis-cli> BLPOP mylist 0
1) mylist
2) d
```

#### Sorted Sets (ZSET)

```text
mini-redis-cli> ZADD z 10 alice
(integer) 1
mini-redis-cli> ZADD z 20 bob
(integer) 1
mini-redis-cli> ZADD z 15 carol
(integer) 1

# Rank is zero-based; returns (nil) if not present
mini-redis-cli> ZRANK z carol
(integer) 1

# Range by index (supports negative indexes)
mini-redis-cli> ZRANGE z 0 -1
1) alice
2) carol
3) bob

# Cardinality
mini-redis-cli> ZCARD z
(integer) 3

# Score or (nil) if member missing
mini-redis-cli> ZSCORE z bob
20

# Remove member
mini-redis-cli> ZREM z bob
(integer) 
```


## Resources

- [Redis Commands](https://redis.io/docs/latest/commands//?group=bf)
- [Redis DataTypes](https://redis.io/docs/latest/develop/data-types/)
- [Go Concurrency](https://go.dev/tour/concurrency/1)
- [Redis serialization protocol](https://redis.io/docs/latest/develop/reference/protocol-spec/)
- [Redis Persistence](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/)


## Contributing

Contributions are welcome! To propose changes:

1. Fork the repo and create a feature branch.
2. Make your changes with clear commit messages.
3. Open a Pull Request describing the change and rationale. Include examples when applicable.
