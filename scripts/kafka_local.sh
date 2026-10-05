#!/usr/bin/env bash
# One local Apache Kafka broker in KRaft mode for the stream experiments
# (k1 to k4) and for running the pipeline by hand.
#
#   scripts/kafka_local.sh start   download Kafka 3.9.1 if needed, format, start on 127.0.0.1:19092
#   scripts/kafka_local.sh stop
#   scripts/kafka_local.sh reset   stop and delete the broker's data
#
# Kafka needs Java 17 or later on PATH, or JAVA_HOME. Everything lives under
# build/ (ignored by git): the Kafka distribution, its data and its log.
# On Windows (Git Bash) it uses Kafka's bin/windows scripts. A Docker
# alternative: docker run -d --name riskgate-kafka -p 19092:9092 apache/kafka:3.9.1
# with KAFKA_ADVERTISED_LISTENERS=PLAINTEXT://127.0.0.1:19092.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD
VER=3.9.1
DIST=build/tools/kafka_2.13-$VER
DATA=build/kafka-data
CONF=build/kafka-server.properties
LOG=build/kafka.log
PORT=${KAFKA_PORT:-19092}

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) WIN=1 ;;
  *) WIN=0 ;;
esac
if [[ -z ${JAVA_HOME:-} && -d build/tools ]]; then
  JDK=$(ls -d build/tools/jdk-* 2>/dev/null | head -1 || true)
  [[ -n $JDK ]] && export JAVA_HOME=$ROOT/$JDK
fi

fetch() {
  [[ -d $DIST ]] && return
  mkdir -p build/tools
  curl -sSL -o build/tools/kafka.tgz "https://archive.apache.org/dist/kafka/$VER/kafka_2.13-$VER.tgz"
  tar -xzf build/tools/kafka.tgz -C build/tools
  rm build/tools/kafka.tgz
}

path() { if [[ $WIN == 1 ]]; then cygpath -m "$1"; else echo "$1"; fi; }

conf() {
  mkdir -p "$DATA"
  cat >"$CONF" <<EOF
process.roles=broker,controller
node.id=1
controller.quorum.voters=1@127.0.0.1:$((PORT + 1))
listeners=PLAINTEXT://127.0.0.1:$PORT,CONTROLLER://127.0.0.1:$((PORT + 1))
advertised.listeners=PLAINTEXT://127.0.0.1:$PORT
controller.listener.names=CONTROLLER
listener.security.protocol.map=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT
inter.broker.listener.name=PLAINTEXT
log.dirs=$(path "$ROOT/$DATA")
num.partitions=1
offsets.topic.replication.factor=1
offsets.topic.num.partitions=8
transaction.state.log.replication.factor=1
transaction.state.log.min.isr=1
group.initial.rebalance.delay.ms=0
auto.create.topics.enable=false
log.retention.hours=168
EOF
  # Kafka on Windows fails a log directory when it renames segment files to
  # delete or compact them (KAFKA-1194), so the broker never does either
  # there: topics are not deleted, and the decisions topic is read back
  # deduplicated by payment id rather than compacted.
  if [[ $WIN == 1 ]]; then
    printf '%s
' log.cleaner.enable=false log.retention.hours=-1 delete.topic.enable=false >>"$CONF"
  fi
}

# On Windows the .bat launchers build a classpath longer than cmd.exe
# accepts, so Java is started directly with a wildcard classpath.
kjava() {
  local main=$1; shift
  local java=java
  [[ -n ${JAVA_HOME:-} ]] && java=$JAVA_HOME/bin/java
  "$java" $KAFKA_HEAP_OPTS -cp "$(path "$ROOT/$DIST")/libs/*" \
    -Dlog4j.configuration="file:$(path "$ROOT/$DIST")/config/log4j.properties" \
    -Dkafka.logs.dir="$(path "$ROOT/build/kafka-logs")" "$main" "$@"
}

case "${1:-start}" in
  start)
    fetch
    conf
    export KAFKA_HEAP_OPTS=${KAFKA_HEAP_OPTS:--Xms512m -Xmx1g}
    if [[ ! -f $DATA/meta.properties ]]; then
      KAFKA_HEAP_OPTS=-Xmx256m kjava kafka.tools.StorageTool format -t "riskgate-local-0000001" -c "$(path "$ROOT/$CONF")" >/dev/null
    fi
    nohup bash -c "$(declare -f kjava path); ROOT='$ROOT' DIST='$DIST' WIN=$WIN KAFKA_HEAP_OPTS='$KAFKA_HEAP_OPTS' JAVA_HOME='${JAVA_HOME:-}'; kjava kafka.Kafka '$(path "$ROOT/$CONF")'" >"$LOG" 2>&1 &
    for _ in $(seq 1 60); do
      if grep -q "Kafka Server started" "$LOG" 2>/dev/null; then echo "kafka up on 127.0.0.1:$PORT"; exit 0; fi
      sleep 1
    done
    echo "kafka did not start; see $LOG" >&2
    exit 1
    ;;
  stop)
    if [[ $WIN == 1 ]]; then
      powershell.exe -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='java.exe'\" | Where-Object { \$_.CommandLine -like '*kafka-server.properties*' } | ForEach-Object { Stop-Process -Id \$_.ProcessId -Force }"
    else
      pkill -f kafka-server.properties || true
    fi
    ;;
  reset)
    "$0" stop || true
    rm -rf "$DATA"
    ;;
  *) echo "usage: $0 start|stop|reset" >&2; exit 2 ;;
esac
