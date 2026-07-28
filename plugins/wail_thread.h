// Minimal thread/mutex/sleep shim for the WAIL CLAP plugins and their test
// harness — Win32 API on Windows, pthreads elsewhere.
#ifndef WAIL_THREAD_H
#define WAIL_THREAD_H

#include <stdlib.h>
#include <time.h>

#ifdef _WIN32
// Pulled in explicitly: this header has no socket includes to drag it in.
#define WIN32_LEAN_AND_MEAN
#include <windows.h>

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

#endif // WAIL_THREAD_H
