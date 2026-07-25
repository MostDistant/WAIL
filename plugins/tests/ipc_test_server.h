// Test double for the WAIL app's side of the plugin loopback IPC
// (wail-app/ipc.go): the app is the TCP server; plugins connect, announce a role
// byte, then exchange length-prefixed frames. This header lets a test play the
// app's role — accept the plugin, read its handshake + frames, push frames back —
// so the plugins can be integration-tested without a DAW or the Go app.
#pragma once

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <deque>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#ifndef _WIN32
#include <fcntl.h>
#endif

#include <clap/clap.h> // wail_ipc.h's state-stream helpers reference clap_*stream_t
#include "wail_ipc.h"

namespace wailtest {

// (defined in the codecs section below)
inline bool decodeTrackName(const std::vector<uint8_t> &p, uint16_t &streamIndex, std::string &name);

// unusedPort returns a loopback port that was free a moment ago — for tests that
// want the plugin's connection attempts to be refused.
inline int unusedPort() {
#ifdef _WIN32
   WSADATA wsa;
   WSAStartup(MAKEWORD(2, 2), &wsa);
#endif
   wail_sock s = socket(AF_INET, SOCK_STREAM, 0);
   if (s == WAIL_INVALID_SOCK) return 0;
   sockaddr_in addr{};
   addr.sin_family = AF_INET;
   addr.sin_port = 0;
   inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);
   if (bind(s, (sockaddr *)&addr, sizeof(addr)) != 0) {
      wail_sock_close(s);
      return 0;
   }
   socklen_t alen = sizeof(addr);
   int port = 0;
   if (getsockname(s, (sockaddr *)&addr, &alen) == 0) port = ntohs(addr.sin_port);
   wail_sock_close(s);
   return port;
}

// IpcTestServer listens on an ephemeral loopback port and speaks the plugin IPC
// wire format ([u32 LE length][payload], payload[0] = tag). One client at a time:
// the accept loop serves each connection to completion before accepting the next,
// which matches the tests' single-plugin usage.
class IpcTestServer {
public:
   IpcTestServer() = default;
   ~IpcTestServer() { stop(); }
   IpcTestServer(const IpcTestServer &) = delete;
   IpcTestServer &operator=(const IpcTestServer &) = delete;

   // start binds 127.0.0.1:0 and begins accepting. Returns the bound port (0 on error).
   int start() {
#ifdef _WIN32
      WSADATA wsa;
      if (WSAStartup(MAKEWORD(2, 2), &wsa) != 0) return 0;
#endif
      listener_ = socket(AF_INET, SOCK_STREAM, 0);
      if (listener_ == WAIL_INVALID_SOCK) return 0;
      int one = 1;
      setsockopt(listener_, SOL_SOCKET, SO_REUSEADDR, (const char *)&one, sizeof(one));
      // Non-blocking so the accept loop polls and can observe stop(); closing a
      // listening socket does not reliably unblock a parked accept() on macOS.
#ifdef _WIN32
      u_long nb = 1;
      ioctlsocket(listener_, FIONBIO, &nb);
#else
      int fl = fcntl(listener_, F_GETFL, 0);
      fcntl(listener_, F_SETFL, fl | O_NONBLOCK);
#endif
      sockaddr_in addr{};
      addr.sin_family = AF_INET;
      addr.sin_port = 0;
      inet_pton(AF_INET, "127.0.0.1", &addr.sin_addr);
      if (bind(listener_, (sockaddr *)&addr, sizeof(addr)) != 0) return 0;
      if (listen(listener_, 4) != 0) return 0;
      socklen_t alen = sizeof(addr);
      if (getsockname(listener_, (sockaddr *)&addr, &alen) != 0) return 0;
      port_ = ntohs(addr.sin_port);
      running_ = true;
      acceptThread_ = std::thread([this] { acceptLoop(); });
      return port_;
   }

   void stop() {
      if (!running_.exchange(false)) return;
      {
         std::lock_guard<std::mutex> lk(mu_);
         if (client_ != WAIL_INVALID_SOCK) {
            // shutdown (not close) unblocks serveClient's recv; the fd is closed
            // after the thread joins so it can't be reused from under it.
#ifdef _WIN32
            shutdown(client_, SD_BOTH);
#else
            shutdown(client_, SHUT_RDWR);
#endif
         }
      }
      if (acceptThread_.joinable()) acceptThread_.join();
      std::lock_guard<std::mutex> lk(mu_);
      if (client_ != WAIL_INVALID_SOCK) {
         wail_sock_close(client_);
         client_ = WAIL_INVALID_SOCK;
      }
      if (listener_ != WAIL_INVALID_SOCK) {
         wail_sock_close(listener_);
         listener_ = WAIL_INVALID_SOCK;
      }
   }

   int port() const { return port_; }

   bool waitConnected(int timeoutMs) {
      std::unique_lock<std::mutex> lk(mu_);
      return cv_.wait_for(lk, std::chrono::milliseconds(timeoutMs), [&] { return connected_; });
   }

   // Handshake fields, valid after waitConnected().
   int role() {
      std::lock_guard<std::mutex> lk(mu_);
      return role_;
   }
   uint16_t streamIndex() {
      std::lock_guard<std::mutex> lk(mu_);
      return streamIndex_;
   }

   bool waitFrameCount(size_t n, int timeoutMs) {
      std::unique_lock<std::mutex> lk(mu_);
      return cv_.wait_for(lk, std::chrono::milliseconds(timeoutMs), [&] { return frames_.size() >= n; });
   }

   // waitTrackName blocks until a TrackName frame (Send → App, mirrors
   // DecodeTrackName in wail-app/ipc.go) arrives carrying exactly `name`.
   bool waitTrackName(const std::string &name, int timeoutMs) {
      std::unique_lock<std::mutex> lk(mu_);
      return cv_.wait_for(lk, std::chrono::milliseconds(timeoutMs), [&] {
         for (const auto &f : frames_) {
            uint16_t idx;
            std::string n;
            if (decodeTrackName(f, idx, n) && n == name) return true;
         }
         return false;
      });
   }

   // hasTag reports whether any received frame carries the given IPC tag.
   bool hasTag(uint8_t tag) {
      std::lock_guard<std::mutex> lk(mu_);
      for (const auto &f : frames_)
         if (!f.empty() && f[0] == tag) return true;
      return false;
   }

   // frames returns a snapshot of every payload received so far (never consumed).
   std::vector<std::vector<uint8_t>> frames() {
      std::lock_guard<std::mutex> lk(mu_);
      return {frames_.begin(), frames_.end()};
   }

   // sendFrame writes one framed payload to the connected plugin.
   bool sendFrame(const std::vector<uint8_t> &payload) {
      std::lock_guard<std::mutex> lk(mu_);
      if (client_ == WAIL_INVALID_SOCK || !connected_) return false;
      return wail_send_frame(client_, payload.data(), (uint32_t)payload.size()) == 0;
   }

private:
   void acceptLoop() {
      while (running_) {
         wail_sock c = accept(listener_, nullptr, nullptr);
         if (c == WAIL_INVALID_SOCK) {
            wail_sleep_ms(10);
            continue;
         }
         // macOS/BSD accepted sockets inherit O_NONBLOCK from the listener
         // (unlike Linux) — serveClient wants blocking reads.
#ifdef _WIN32
         u_long nb = 0;
         ioctlsocket(c, FIONBIO, &nb);
#else
         int cfl = fcntl(c, F_GETFL, 0);
         fcntl(c, F_SETFL, cfl & ~O_NONBLOCK);
#endif
         {
            std::lock_guard<std::mutex> lk(mu_);
            client_ = c;
         }
         serveClient(c);
         std::lock_guard<std::mutex> lk(mu_);
         client_ = WAIL_INVALID_SOCK;
         connected_ = false;
      }
   }

   static bool readExact(wail_sock c, uint8_t *buf, size_t len) {
      size_t got = 0;
      while (got < len) {
#ifdef _WIN32
         int n = recv(c, (char *)(buf + got), (int)(len - got), 0);
         if (n < 0 && WSAGetLastError() == WSAEWOULDBLOCK) continue;
         if (n <= 0) return false;
#else
         ssize_t n = recv(c, buf + got, len - got, 0);
         if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) continue; // see O_NONBLOCK note above
         if (n <= 0) return false;
#endif
         got += (size_t)n;
      }
      return true;
   }

   void serveClient(wail_sock c) {
      uint8_t role = 0;
      if (!readExact(c, &role, 1)) return;
      uint16_t sidx = 0;
      if (role == WAIL_IPC_ROLE_SEND) { // send plugins append their stream index
         uint8_t b[2];
         if (!readExact(c, b, 2)) return;
         sidx = wail_get_u16(b);
      }
      {
         std::lock_guard<std::mutex> lk(mu_);
         role_ = role;
         streamIndex_ = sidx;
         connected_ = true;
      }
      cv_.notify_all();
      for (;;) {
         uint8_t hdr[4];
         if (!readExact(c, hdr, 4)) return;
         uint32_t len = wail_get_u32(hdr);
         if (len > (16u << 20)) return; // same framing-violation cap as the app
         std::vector<uint8_t> payload(len);
         if (len && !readExact(c, payload.data(), len)) return;
         {
            std::lock_guard<std::mutex> lk(mu_);
            frames_.push_back(std::move(payload));
         }
         cv_.notify_all();
      }
   }

   std::atomic<bool> running_{false};
   wail_sock listener_ = WAIL_INVALID_SOCK;
   wail_sock client_ = WAIL_INVALID_SOCK;
   int port_ = 0;
   std::thread acceptThread_;

   std::mutex mu_;
   std::condition_variable cv_;
   bool connected_ = false;
   int role_ = -1;
   uint16_t streamIndex_ = 0;
   std::deque<std::vector<uint8_t>> frames_;
};

// --- message codecs (mirrors wail-app/ipc.go) ---

inline void putU16(std::vector<uint8_t> &b, uint16_t v) {
   b.push_back((uint8_t)v);
   b.push_back((uint8_t)(v >> 8));
}
inline void putU32(std::vector<uint8_t> &b, uint32_t v) {
   for (int i = 0; i < 4; i++) b.push_back((uint8_t)(v >> (8 * i)));
}
inline void putU64(std::vector<uint8_t> &b, uint64_t v) {
   for (int i = 0; i < 8; i++) b.push_back((uint8_t)(v >> (8 * i)));
}
inline void putStr8(std::vector<uint8_t> &b, const std::string &s) {
   size_t n = s.size() > 255 ? 255 : s.size();
   b.push_back((uint8_t)n);
   b.insert(b.end(), s.begin(), s.begin() + (ptrdiff_t)n);
}

// encodeRemotePCM mirrors EncodeRemotePCM: App → Recv PCM for one remote stream.
// playAtMicros is the monotonic-µs instant the first frame should play (0 or a
// small value = legacy/unstamped → FIFO playback).
inline std::vector<uint8_t> encodeRemotePCM(const std::string &peerID, uint16_t streamID,
                                            uint8_t channels, uint32_t sampleRate,
                                            int64_t playAtMicros,
                                            const std::vector<int16_t> &samples) {
   std::vector<uint8_t> m;
   m.push_back(WAIL_TAG_REMOTEPCM);
   putStr8(m, peerID);
   putU16(m, streamID);
   m.push_back(channels);
   putU32(m, sampleRate);
   putU64(m, (uint64_t)playAtMicros);
   for (int16_t s : samples) putU16(m, (uint16_t)s);
   return m;
}

// encodeStreamName mirrors EncodeStreamName: App → Recv port display name.
inline std::vector<uint8_t> encodeStreamName(const std::string &peerID, uint16_t streamID,
                                             const std::string &name) {
   std::vector<uint8_t> m;
   m.push_back(WAIL_TAG_STREAMNAME);
   putStr8(m, peerID);
   putU16(m, streamID);
   putU16(m, (uint16_t)(name.size() > 65535 ? 65535 : name.size()));
   m.insert(m.end(), name.begin(), name.end());
   return m;
}

// encodeStreamGone mirrors EncodeStreamGone: App → Recv "stream ended, free its port".
inline std::vector<uint8_t> encodeStreamGone(const std::string &peerID, uint16_t streamID) {
   std::vector<uint8_t> m;
   m.push_back(WAIL_TAG_STREAMGONE);
   putStr8(m, peerID);
   putU16(m, streamID);
   return m;
}

// decodeTrackName mirrors DecodeTrackName: Send → App DAW track name for a
// plugin stream (tag + u16 stream index + str16 name).
inline bool decodeTrackName(const std::vector<uint8_t> &p, uint16_t &streamIndex, std::string &name) {
   if (p.size() < 5 || p[0] != WAIL_TAG_TRACKNAME) return false;
   streamIndex = wail_get_u16(&p[1]);
   uint16_t n = wail_get_u16(&p[3]);
   if (p.size() < 5u + n) return false;
   name.assign((const char *)p.data() + 5, n);
   return true;
}

// RawPCM is a decoded Send → App block (mirrors DecodeRawPCM).
struct RawPCM {
   uint16_t streamIndex = 0;
   uint8_t flags = 0;
   uint8_t channels = 0;
   uint32_t sampleRate = 0;
   uint64_t frameCounter = 0;
   std::vector<float> pcm; // float32 LE interleaved (the plugin never sends int16)
};

inline bool decodeRawPCM(const std::vector<uint8_t> &p, RawPCM &out) {
   if (p.size() < 17 || p[0] != WAIL_TAG_RAWPCM) return false;
   out.streamIndex = wail_get_u16(&p[1]);
   out.flags = p[3];
   out.channels = p[4];
   out.sampleRate = wail_get_u32(&p[5]);
   out.frameCounter = (uint64_t)wail_get_i64(&p[9]);
   size_t bytes = p.size() - 17;
   if (bytes % sizeof(float) != 0) return false;
   out.pcm.resize(bytes / sizeof(float));
   std::memcpy(out.pcm.data(), p.data() + 17, bytes);
   return true;
}

} // namespace wailtest
