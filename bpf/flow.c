//go:build ignore

// TCP flow probe. No per-packet hooks: one tracepoint on socket state changes
// plus an iterator that user space runs periodically over all open sockets.

#include <linux/bpf.h>
#include <linux/types.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_endian.h>

char LICENSE[] SEC("license") = "GPL";

#define AF_INET 2
#define IPPROTO_TCP 6
#define TCP_ESTABLISHED 1
#define TCP_SYN_SENT 2
#define TCP_SYN_RECV 3
#define TCP_CLOSE 7

// Minimal kernel types; CO-RE relocates the field offsets at load time.
#define CORE __attribute__((preserve_access_index))

struct in6_addr {
	union {
		__be32 u6_addr32[4];
	} in6_u;
} CORE;

struct sock_common {
	__be32 skc_daddr;
	__be32 skc_rcv_saddr;
	__be16 skc_dport;
	__u16 skc_num;
	unsigned short skc_family;
	struct in6_addr skc_v6_daddr;
	struct in6_addr skc_v6_rcv_saddr;
} CORE;

typedef struct {
	__u32 val;
} CORE kuid_t;

struct sock {
	struct sock_common __sk_common;
	__u16 sk_protocol;
	kuid_t sk_uid;
} CORE;

struct tcp_sock {
	__u64 bytes_received;
	__u64 bytes_sent;
	__u32 total_retrans;
	__u32 srtt_us;
} CORE;

struct seq_file;

struct bpf_iter_meta {
	struct seq_file *seq;
} CORE;

struct bpf_iter__bpf_sk_storage_map {
	struct bpf_iter_meta *meta;
	struct sock *sk;
	void *value;
} CORE;

enum { KIND_TICK, KIND_OPEN, KIND_CLOSE, KIND_FAILED };
enum { ROLE_CLIENT, ROLE_SERVER };

// Per-socket state: counters as of the last report, so every record carries deltas.
struct conn {
	__u64 start_ns;
	__u64 sent;
	__u64 recv;
	__u32 retrans;
	__u16 role;
	__u16 sport; // saved at open: the kernel clears the local port before the close event
};

struct rec {
	__u8 saddr[16]; // IPv4 as v4-mapped IPv6
	__u8 daddr[16];
	__u64 sent; // deltas since the previous record of this socket
	__u64 recv;
	__u64 dur_ns;
	__u32 retrans;
	__u32 srtt_us;
	__u32 uid;
	__u16 sport;
	__u16 dport;
	__u8 role;
	__u8 kind;
	__u8 pad[6];
};
const struct rec *unused_rec __attribute__((unused)); // keeps the type in BTF for bpf2go

struct {
	__uint(type, BPF_MAP_TYPE_SK_STORAGE);
	__uint(map_flags, BPF_F_NO_PREALLOC);
	__type(key, int);
	__type(value, struct conn);
} conns SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 20);
} events SEC(".maps");

static __always_inline void fill(struct rec *r, struct sock *sk, struct conn *c, __u8 kind)
{
	__u64 sent = 0, recv = 0;
	__u32 retrans = 0;
	struct tcp_sock *tp = bpf_skc_to_tcp_sock(sk);

	__builtin_memset(r, 0, sizeof(*r));
	r->kind = kind;
	r->uid = sk->sk_uid.val;
	r->sport = sk->__sk_common.skc_num;
	r->dport = bpf_ntohs(sk->__sk_common.skc_dport);

	if (sk->__sk_common.skc_family == AF_INET) {
		__be32 s = sk->__sk_common.skc_rcv_saddr, d = sk->__sk_common.skc_daddr;

		r->saddr[10] = r->saddr[11] = r->daddr[10] = r->daddr[11] = 0xff;
		__builtin_memcpy(&r->saddr[12], &s, 4);
		__builtin_memcpy(&r->daddr[12], &d, 4);
	} else {
		for (int i = 0; i < 4; i++) {
			__be32 s = sk->__sk_common.skc_v6_rcv_saddr.in6_u.u6_addr32[i];
			__be32 d = sk->__sk_common.skc_v6_daddr.in6_u.u6_addr32[i];

			__builtin_memcpy(&r->saddr[i * 4], &s, 4);
			__builtin_memcpy(&r->daddr[i * 4], &d, 4);
		}
	}

	if (tp) {
		sent = tp->bytes_sent;
		recv = tp->bytes_received;
		retrans = tp->total_retrans;
		r->srtt_us = tp->srtt_us >> 3; // kernel stores srtt << 3
	}
	if (c) {
		r->role = c->role;
		r->sport = c->sport;
		r->dur_ns = bpf_ktime_get_ns() - c->start_ns;
		r->sent = sent - c->sent;
		r->recv = recv - c->recv;
		r->retrans = retrans - c->retrans;
		c->sent = sent;
		c->recv = recv;
		c->retrans = retrans;
	}
}

SEC("tp_btf/inet_sock_set_state")
int BPF_PROG(set_state, struct sock *sk, int oldstate, int newstate)
{
	struct rec r;
	struct conn *c;

	if (!sk || sk->sk_protocol != IPPROTO_TCP)
		return 0;

	if (newstate == TCP_ESTABLISHED) {
		c = bpf_sk_storage_get(&conns, sk, 0, BPF_SK_STORAGE_GET_F_CREATE);
		if (!c)
			return 0;
		c->start_ns = bpf_ktime_get_ns();
		c->role = oldstate == TCP_SYN_RECV ? ROLE_SERVER : ROLE_CLIENT;
		c->sport = sk->__sk_common.skc_num;
		fill(&r, sk, c, KIND_OPEN);
	} else if (newstate == TCP_CLOSE) {
		c = bpf_sk_storage_get(&conns, sk, 0, 0);
		if (c) {
			fill(&r, sk, c, KIND_CLOSE);
			bpf_sk_storage_delete(&conns, sk); // the iterator must not report it again
		} else if (oldstate == TCP_SYN_SENT) {
			fill(&r, sk, 0, KIND_FAILED);
		} else {
			return 0;
		}
	} else {
		return 0;
	}

	// shortcut: one wakeup per event, fine at two events per connection; batch
	// with BPF_RB_NO_WAKEUP if connection churn gets into the 100k/s range.
	bpf_ringbuf_output(&events, &r, sizeof(r), 0);
	return 0;
}

// Run from user space every interval: one record per open connection.
SEC("iter/bpf_sk_storage_map")
int dump(struct bpf_iter__bpf_sk_storage_map *ctx)
{
	struct sock *sk = ctx->sk;
	struct conn *c = ctx->value;
	struct rec r;

	if (!sk || !c)
		return 0;
	fill(&r, sk, c, KIND_TICK);
	bpf_seq_write(ctx->meta->seq, &r, sizeof(r));
	return 0;
}
