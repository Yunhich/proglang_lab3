package main

import (
	"context"
	"crypto/sha3"
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Token struct {
	Data          string
	ReceiverHash  [32]byte // sha3-256
	TTL           int
	OriginNodeID  int
	ReceiverNode  int // только для удобства логов (не требуется протоколом)
	CreatedAtUnix int64
}

func hashID(id int) [32]byte {
	// Хэшируем строку с номером узла (стабильно и просто)
	b := []byte(fmt.Sprintf("%d", id))
	return sha3.Sum256(b)
}

func shortHash(h [32]byte) string {
	return hex.EncodeToString(h[:4]) // первые 4 байта = 8 hex-символов
}

func randData(r *rand.Rand, from int) string {
	words := []string{
		"alpha", "bravo", "charlie", "delta", "echo",
		"foxtrot", "golf", "hotel", "india", "juliet",
		"kilo", "lima", "mike", "november", "oscar",
		"papa", "quebec", "romeo", "sierra", "tango",
		"uniform", "victor", "whiskey", "xray", "yankee", "zulu",
	}
	n := 2 + r.Intn(4)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("from=%d:", from))
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte('-')
		}
		sb.WriteString(words[r.Intn(len(words))])
	}
	sb.WriteString(fmt.Sprintf(":%d", r.Intn(10000)))
	return sb.String()
}

func newToken(r *rand.Rand, originID, receiverID int, ttl int) Token {
	h := hashID(receiverID)
	return Token{
		Data:          randData(r, originID),
		ReceiverHash:  h,
		TTL:           ttl,
		OriginNodeID:  originID,
		ReceiverNode:  receiverID, // для логов
		CreatedAtUnix: time.Now().Unix(),
	}
}

func chooseReceiver(r *rand.Rand, nodes int, notID int) int {
	if nodes <= 1 {
		return 0
	}
	for {
		x := r.Intn(nodes)
		if x != notID {
			return x
		}
	}
}

func nodeLoop(ctx context.Context, wg *sync.WaitGroup, id int, in <-chan Token, out chan<- Token, nodes int, logMu *sync.Mutex) {
	defer wg.Done()

	// Отдельный RNG на узел (чтобы не делить глобальный)
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*1_000_000))

	selfHash := hashID(id)

	log := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Printf(format+"\n", args...)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case tok := <-in:
			// Проверка доставки
			if tok.ReceiverHash == selfHash {
				log("[DELIVER] node=%d got token {origin=%d -> receiver=%d hash=%s ttl=%d data=%q}",
					id, tok.OriginNodeID, tok.ReceiverNode, shortHash(tok.ReceiverHash), tok.TTL, tok.Data)

				// Получивший сообщение генерирует следующее произвольное сообщение и выбирает произвольного получателя
				recv := chooseReceiver(r, nodes, id)
				ttl := 2 + r.Intn(nodes*2+1) // чтобы иногда успевало, иногда сгорала жизнь
				next := newToken(r, id, recv, ttl)

				log("[NEW]     node=%d created token {origin=%d -> receiver=%d hash=%s ttl=%d data=%q}",
					id, next.OriginNodeID, next.ReceiverNode, shortHash(next.ReceiverHash), next.TTL, next.Data)

				select {
				case <-ctx.Done():
					return
				case out <- next:
				}
				continue
			}

			// Не адресат — пересылка или истечение TTL
			if tok.TTL <= 0 {
				log("[DROP]    node=%d dropped expired token {origin=%d -> receiver=%d hash=%s ttl=%d data=%q}",
					id, tok.OriginNodeID, tok.ReceiverNode, shortHash(tok.ReceiverHash), tok.TTL, tok.Data)

				// Чтобы эмуляция не "умирала", узел, встретивший истекший токен, запускает новый
				recv := chooseReceiver(r, nodes, id)
				ttl := 2 + r.Intn(nodes*2+1)
				next := newToken(r, id, recv, ttl)

				log("[NEW]     node=%d created token (after drop) {origin=%d -> receiver=%d hash=%s ttl=%d data=%q}",
					id, next.OriginNodeID, next.ReceiverNode, shortHash(next.ReceiverHash), next.TTL, next.Data)

				select {
				case <-ctx.Done():
					return
				case out <- next:
				}
				continue
			}

			// ttl -= 1 на каждой пересылке
			tok.TTL--

			log("[FWD]     node=%d -> node=%d forwarding {origin=%d -> receiver=%d hash=%s ttl=%d data=%q}",
				id, (id+1)%nodes, tok.OriginNodeID, tok.ReceiverNode, shortHash(tok.ReceiverHash), tok.TTL, tok.Data)

			select {
			case <-ctx.Done():
				return
			case out <- tok:
			}
		}
	}
}

func main() {
	var n int
	flag.IntVar(&n, "n", 5, "число узлов TokenRing (>=2)")
	flag.Parse()

	if n < 2 {
		fmt.Println("Ошибка: -n должно быть >= 2")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ctrl+C для остановки
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nStopping...")
		cancel()
	}()

	// Каналы по ребрам кольца: out[i] -> in[(i+1)%n]
	ch := make([]chan Token, n)
	for i := 0; i < n; i++ {
		// Небольшой буфер уменьшает "затыки" логов
		ch[i] = make(chan Token, 8)
	}

	var wg sync.WaitGroup
	var logMu sync.Mutex

	// Запуск узлов
	for i := 0; i < n; i++ {
		in := ch[(i-1+n)%n] // получает от i-1
		out := ch[i]        // шлет в i+1 (т.к. ch[i] читается следующим)
		wg.Add(1)
		go nodeLoop(ctx, &wg, i, in, out, n, &logMu)
	}

	// Первое сообщение отправляет основной поток узлу №1
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	firstReceiver := chooseReceiver(r, n, 1)
	firstTTL := 2 + r.Intn(n*2+1)
	first := newToken(r, 1, firstReceiver, firstTTL)

	logMu.Lock()
	fmt.Printf("[INJECT]  main -> node=1 injected token {origin=%d -> receiver=%d hash=%s ttl=%d data=%q}\n",
		first.OriginNodeID, first.ReceiverNode, shortHash(first.ReceiverHash), first.TTL, first.Data)
	logMu.Unlock()

	// Инжектим в "вход" узла 1: это канал от 0 к 1, то есть ch[0]
	select {
	case <-ctx.Done():
		// ничего
	case ch[0] <- first:
	}

	wg.Wait()
}
