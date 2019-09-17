# tx-indexer

## Usage
```
go run main.go -bitcoind=http://<bitcoind rest api endpoint>:8332 -prune=12
```
## Docker run
```
docker build -t index .
```
```
docker run -d -v /var/leveldb:/root/db --name=index -p 9096:9096 index -bitcoind http://<bitcoind rest api endpoint>:8332 -prune 120
```
## bitcoind-node
mainnet
```
docker run -v /var/bitcoind:/bitcoin  --name=bitcoind-node -d -p 0.0.0.0:8333:8333 -p 0.0.0.0:8332:8332 kylemanna/bitcoind --prune=1812 -rest -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0
```
testnet
```
docker run -v /var/bitcoind:/bitcoin  --name=bitcoind-node -d -p 0.0.0.0:18333:18333 -p 0.0.0.0:18332:18332 kylemanna/bitcoind --prune=1812 -testnet=1 -rest -rpcbind=0.0.0.0 -rpcallowip=0.0.0.0/0
```