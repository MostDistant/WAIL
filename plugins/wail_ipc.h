// Shared loopback-TCP IPC helpers for the WAIL CLAP plugins (ADR-0005).
// The plugin is a thin raw-PCM bridge: the WAIL app owns all Opus/WAIF/interval
// logic, so this only frames raw PCM + small control messages. Wire format matches
// wail-app/ipc.go exactly: every message is [u32 LE length][payload], payload[0] is
// the tag. All multi-byte fields are little-endian (all target platforms are LE).
#ifndef WAIL_IPC_H
#define WAIL_IPC_H

#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#ifdef _WIN32
#include <winsock2.h>
#include <ws2tcpip.h>
typedef SOCKET wail_sock;
#define WAIL_INVALID_SOCK INVALID_SOCKET
#else
#include <arpa/inet.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>
typedef int wail_sock;
#define WAIL_INVALID_SOCK (-1)
#endif

// --- minimal thread + mutex abstraction (Windows API vs pthreads) ---
// The IPC thread runs a `void *fn(void *)`; on Windows it's wrapped in a trampoline.
#ifdef _WIN32
typedef HANDLE wail_thread;
typedef CRITICAL_SECTION wail_mutex;
typedef struct {
   void *(*fn)(void *);
   void *arg;
} wail_thunk;
static DWORD WINAPI wail_thread_trampoline(LPVOID p) {
   wail_thunk t = *(wail_thunk *)p;
   free(p);
   t.fn(t.arg);
   return 0;
}
static inline int wail_thread_create(wail_thread *th, void *(*fn)(void *), void *arg) {
   wail_thunk *t = (wail_thunk *)malloc(sizeof(*t));
   if (!t) return -1;
   t->fn = fn;
   t->arg = arg;
   HANDLE h = CreateThread(NULL, 0, wail_thread_trampoline, t, 0, NULL);
   if (!h) {
      free(t);
      return -1;
   }
   *th = h;
   return 0;
}
static inline void wail_thread_join(wail_thread th) {
   WaitForSingleObject(th, INFINITE);
   CloseHandle(th);
}
static inline void wail_mutex_init(wail_mutex *m) { InitializeCriticalSection(m); }
static inline void wail_mutex_lock(wail_mutex *m) { EnterCriticalSection(m); }
static inline void wail_mutex_unlock(wail_mutex *m) { LeaveCriticalSection(m); }
static inline void wail_mutex_destroy(wail_mutex *m) { DeleteCriticalSection(m); }
#else
#include <pthread.h>
typedef pthread_t wail_thread;
typedef pthread_mutex_t wail_mutex;
static inline int wail_thread_create(wail_thread *th, void *(*fn)(void *), void *arg) {
   return pthread_create(th, NULL, fn, arg);
}
static inline void wail_thread_join(wail_thread th) { pthread_join(th, NULL); }
static inline void wail_mutex_init(wail_mutex *m) { pthread_mutex_init(m, NULL); }
static inline void wail_mutex_lock(wail_mutex *m) { pthread_mutex_lock(m); }
static inline void wail_mutex_unlock(wail_mutex *m) { pthread_mutex_unlock(m); }
static inline void wail_mutex_destroy(wail_mutex *m) { pthread_mutex_destroy(m); }
#endif

// Role bytes (first byte a plugin writes on connect).
#define WAIL_IPC_ROLE_SEND 0x00
#define WAIL_IPC_ROLE_RECV 0x01

// Message tags.
#define WAIL_TAG_RAWPCM 0x10
#define WAIL_TAG_REMOTEPCM 0x11
#define WAIL_TAG_STREAMNAME 0x12
#define WAIL_TAG_STREAMGONE 0x13
#define WAIL_TAG_METRICS 0x06

// RawPCM flag bits.
#define WAIL_RAW_FLAG_I16 0x01
#define WAIL_RAW_FLAG_PLAYING 0x02

#define WAIL_IPC_DEFAULT_HOST "127.0.0.1"
#define WAIL_IPC_DEFAULT_PORT 9191

// --- little-endian writers (append at *off, advance it) ---

static inline void wail_put_u16(uint8_t *b, size_t *off, uint16_t v) {
   b[*off] = (uint8_t)(v);
   b[*off + 1] = (uint8_t)(v >> 8);
   *off += 2;
}
static inline void wail_put_u32(uint8_t *b, size_t *off, uint32_t v) {
   for (int i = 0; i < 4; i++) b[*off + i] = (uint8_t)(v >> (8 * i));
   *off += 4;
}
static inline void wail_put_u64(uint8_t *b, size_t *off, uint64_t v) {
   for (int i = 0; i < 8; i++) b[*off + i] = (uint8_t)(v >> (8 * i));
   *off += 8;
}

// --- little-endian readers ---

static inline uint16_t wail_get_u16(const uint8_t *b) {
   return (uint16_t)b[0] | ((uint16_t)b[1] << 8);
}
static inline uint32_t wail_get_u32(const uint8_t *b) {
   return (uint32_t)b[0] | ((uint32_t)b[1] << 8) | ((uint32_t)b[2] << 16) | ((uint32_t)b[3] << 24);
}
static inline int64_t wail_get_i64(const uint8_t *b) {
   uint64_t v = 0;
   for (int i = 0; i < 8; i++) v |= (uint64_t)b[i] << (8 * i);
   return (int64_t)v;
}

// --- CLAP stream helpers (clap.state) ---
// The stream contract allows partial reads/writes, so loop until the whole
// buffer moves. 0 (EOF) and negative (error) both fail the operation.

static inline int wail_stream_write_all(const clap_ostream_t *stream, const uint8_t *buf, size_t len) {
   size_t off = 0;
   while (off < len) {
      int64_t n = stream->write(stream, buf + off, (uint64_t)(len - off));
      if (n <= 0) return 0;
      off += (size_t)n;
   }
   return 1;
}
static inline int wail_stream_read_all(const clap_istream_t *stream, uint8_t *buf, size_t len) {
   size_t off = 0;
   while (off < len) {
      int64_t n = stream->read(stream, buf + off, (uint64_t)(len - off));
      if (n <= 0) return 0;
      off += (size_t)n;
   }
   return 1;
}

// --- socket helpers ---

// wail_sock_set_recv_timeout_ms makes a blocking recv() return periodically (with a
// would-block error) so the IPC thread can observe a shutdown request and exit —
// otherwise recv() parks forever on an idle-but-open connection and join() hangs.
static inline void wail_sock_set_recv_timeout_ms(wail_sock s, int ms) {
#ifdef _WIN32
   DWORD tv = (DWORD)ms;
   setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, (const char *)&tv, sizeof(tv));
#else
   struct timeval tv;
   tv.tv_sec = ms / 1000;
   tv.tv_usec = (ms % 1000) * 1000;
   setsockopt(s, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));
#endif
}

// wail_sock_timed_out reports whether the last recv() returned due to the recv
// timeout (a benign idle tick) rather than a real error/EOF.
static inline int wail_sock_timed_out(void) {
#ifdef _WIN32
   return WSAGetLastError() == WSAETIMEDOUT;
#else
   return errno == EAGAIN || errno == EWOULDBLOCK;
#endif
}

static inline void wail_sleep_ms(int ms) {
#ifdef _WIN32
   Sleep(ms);
#else
   struct timespec ts;
   ts.tv_sec = ms / 1000;
   ts.tv_nsec = (long)(ms % 1000) * 1000000L;
   nanosleep(&ts, NULL);
#endif
}

static inline void wail_sock_close(wail_sock s) {
#ifdef _WIN32
   closesocket(s);
#else
   close(s);
#endif
}

// wail_sock_connect dials host:port and enables TCP_NODELAY. Returns
// WAIL_INVALID_SOCK on failure.
static inline wail_sock wail_sock_connect(const char *host, int port) {
#ifdef _WIN32
   WSADATA wsa;
   static int wsa_started = 0;
   if (!wsa_started) {
      if (WSAStartup(MAKEWORD(2, 2), &wsa) != 0) return WAIL_INVALID_SOCK;
      wsa_started = 1;
   }
#endif
   wail_sock s = socket(AF_INET, SOCK_STREAM, 0);
   if (s == WAIL_INVALID_SOCK) return WAIL_INVALID_SOCK;
   struct sockaddr_in addr;
   memset(&addr, 0, sizeof(addr));
   addr.sin_family = AF_INET;
   addr.sin_port = htons((uint16_t)port);
   if (inet_pton(AF_INET, host, &addr.sin_addr) != 1) {
      wail_sock_close(s);
      return WAIL_INVALID_SOCK;
   }
   if (connect(s, (struct sockaddr *)&addr, sizeof(addr)) != 0) {
      wail_sock_close(s);
      return WAIL_INVALID_SOCK;
   }
   int one = 1;
   setsockopt(s, IPPROTO_TCP, TCP_NODELAY, (const char *)&one, sizeof(one));
   return s;
}

// wail_sock_write_all writes the whole buffer. Returns 0 on success, -1 on error.
static inline int wail_sock_write_all(wail_sock s, const uint8_t *buf, size_t len) {
   size_t sent = 0;
   while (sent < len) {
#ifdef _WIN32
      int n = send(s, (const char *)(buf + sent), (int)(len - sent), 0);
      if (n == SOCKET_ERROR) return -1;
#else
      ssize_t n = send(s, buf + sent, len - sent, 0);
      if (n < 0) return -1;
#endif
      sent += (size_t)n;
   }
   return 0;
}

// wail_send_frame writes one length-prefixed IPC frame ([u32 LE len][payload]).
static inline int wail_send_frame(wail_sock s, const uint8_t *payload, uint32_t len) {
   uint8_t hdr[4];
   size_t off = 0;
   wail_put_u32(hdr, &off, len);
   if (wail_sock_write_all(s, hdr, 4) != 0) return -1;
   return wail_sock_write_all(s, payload, len);
}

// wail_ipc_resolve reads host/port from the WAIL_IPC_ADDR env ("host:port"),
// falling back to the loopback default. Writes into caller-owned host_out.
static inline void wail_ipc_resolve(char *host_out, size_t host_cap, int *port_out) {
   snprintf(host_out, host_cap, "%s", WAIL_IPC_DEFAULT_HOST);
   *port_out = WAIL_IPC_DEFAULT_PORT;
   const char *env = getenv("WAIL_IPC_ADDR");
   if (!env || !*env) return;
   const char *colon = strrchr(env, ':');
   if (!colon) return;
   size_t hlen = (size_t)(colon - env);
   if (hlen == 0 || hlen >= host_cap) return;
   memcpy(host_out, env, hlen);
   host_out[hlen] = '\0';
   int p = atoi(colon + 1);
   if (p > 0 && p < 65536) *port_out = p;
}

#endif // WAIL_IPC_H
