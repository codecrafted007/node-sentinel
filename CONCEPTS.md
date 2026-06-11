# Concepts — node-sentinel in plain English

This explains *what* node-sentinel is trying to do and *how it decides*, with no jargon. For how the eBPF machinery actually works, see [`HOW.md`](HOW.md). For the formal design, see [`docs/`](docs/).

Each idea below is tagged ✅ **built** or 🔜 **planned**, so you can tell today's behaviour from where we're heading.

---

## The big picture: a shared apartment

A Kubernetes node is like a **shared apartment**. The tenants (pods) share one kitchen (CPU), one bathroom (disk), one front door (network). Most of the time everyone gets along. But every so often **one tenant hogs the kitchen all day**, and everyone else is left waiting to cook.

node-sentinel is the **building manager**: it quietly watches the shared facilities and works out *who's hogging* and *who's suffering* — so the problem can be dealt with instead of everyone just mysteriously being slow.

## Two roles: offender vs. victim ✅ built

Every contention problem has two sides, and we measure them separately:

- **Offender** — the tenant hogging the kitchen. We measure this as **how much CPU a pod actually uses**.
- **Victim** — the tenant left waiting to cook. We measure this as **how long a pod waits its turn for the CPU** ("run-queue latency" — literally, time spent standing in line).

A key subtlety: the hog is often *busy*, not *waiting* — so the victims are the ones who light up the "waiting" meter. That's why we need both measurements to see the whole story.

The same offender/victim idea applies to **every shared facility**, and the manager now watches all three: the **kitchen (CPU)**, the **bathroom (disk I/O)**, and the **front door (network)** — a pod flooding the network makes its neighbours' packets get retransmitted, exactly like the CPU and disk cases. Everything below (learn-the-normal, unusual-and-bad, how-much, confidence) is applied per facility.

## The problem: one rule doesn't fit everyone ✅ (this is today's limitation)

Right now the manager uses **one fixed rule** to decide if a tenant is suffering: "did they wait more than 5 minutes for the stove?"

But "normal" is different for every tenant. Five minutes is a disaster for someone microwaving a quick lunch, but totally fine for someone slow-cooking a stew all afternoon. **One universal number can't be right for everyone** — it's like setting a single speed limit for motorways *and* school zones.

That's the fragile spot we keep hitting, and the next four ideas fix it.

## Idea 1 — Learn each tenant's *own* normal ✅ built

Instead of one rule for everyone, the manager quietly learns each tenant's *usual* behaviour — "flat 3 normally waits 1 minute; flat 7 normally waits 8 minutes" — and only raises a flag when a tenant gets **noticeably worse than their own usual self**.

Think of a **fitness watch**: it learns *your* resting heart rate, so it alerts when *yours* spikes. It doesn't use one number for everybody.

## Idea 2 — Only worry if it's *unusual* AND *actually bad* ✅ built

If we only compared to normal, we'd overreact. A tenant who normally waits 6 seconds suddenly waiting 24 seconds is "4× worse!" — but 24 seconds is still nothing. So we require **both**: the wait must be unusual for that tenant *and* genuinely long.

Heart-rate again: going from 50 to 100 bpm matters while you're **asleep**, not while you **stand up**. You want both signals before you worry.

Same data, old rule vs. new:

| Tenant | Their normal wait | Wait right now | Old rule (fixed) | New rule |
|--------|-------------------|----------------|------------------|----------|
| `redis` (quick lunches) | ~1 min | 9 min | ⚠️ flagged | ⚠️ flagged — 9× normal *and* clearly long ✅ |
| `batch-job` (slow cook) | ~8 min | 9 min | ⚠️ flagged ❌ *wrong* | ✅ ignored — that's normal for it |
| `logger` (instant) | ~6 sec | 24 sec | ✅ ignored | ✅ ignored — 4× worse but still trivial |

The new logic stops crying wolf about tenants who are simply *built* to be slow, and catches the ones that genuinely got worse — **without anyone tuning a number**.

## Idea 3 — Measure *how much* the hog is overusing ✅ built

Today, if a tenant uses even a *sliver* more than their share, we tag them a hog. Too twitchy. Instead we rank by **how badly** they're overusing.

At a shared dinner platter, one person taking an **extra bite** and another **eating half the dish** are both "over their share" — but only one is the problem. Point at the one eating half.

But "share of the dish" alone has a trap: some tenants are *supposed* to use a lot. The building's front desk (the Kubernetes API server) talks to everyone — it's *always* the busiest on the network. Blaming it for being busy is wrong. So we apply **Idea 1 to offenders too**: a tenant is a hog when they're using far more than *their own* normal — a sudden spike — not merely because they're perpetually the biggest. The front desk humming along at its usual volume → not flagged; a batch job that suddenly floods the network → flagged. The key insight: **contention is a *change*, so the culprit is whoever *changed*.** (Before we've learned a tenant's normal, we say "still learning — can't be sure" rather than guess wrong.)

## Idea 4 — Say how *sure* we are (a confidence score) ✅ built (shown, not yet acted on)

Right now the manager just shouts "someone's hogging!" with no sense of how *sure* it is. We want it to say **"I'm 90% sure it's flat 7"** vs. **"something's off, but I'm only 40% sure."**

This matters enormously, because the whole point of node-sentinel is that *eventually* it will take **automatic action** — relocating or evicting the offending pod. You must **never** evict a pod on a hunch. The confidence score is the safety dial: **act only when very sure; just alert a human when unsure.**

It's the difference between a smoke alarm that shrieks at burnt toast, and a guard who only calls the fire brigade when they actually see flames.

---

## Why this order matters

These four ideas turn node-sentinel from *"a gauge you have to hand-calibrate"* into *"a system that learns what normal looks like, only speaks up when something is both unusual and genuinely bad, points at the real hog, and tells you how sure it is."*

That last part — **confidence** — is also the gate that makes safe automatic remediation possible. So it isn't just polish; it's the foundation the "operator" half of node-sentinel (the automatic taint/evict) will stand on.

**Where we are today (✅):** all four ideas are built. The manager stays quiet when the apartment is calm; under real contention it shows each victim's slowdown *relative to its own normal* (e.g. "104× its usual wait"), ranks who's over-using CPU, and reports a confidence score with an honest verdict — including *"I see contention but I'm not sure which tenant; it looks like a building-wide issue"* when it genuinely can't pin it on one pod.

**What's next (🔜):** *acting* on the confidence — the "operator" half of node-sentinel that will automatically move or evict a high-confidence offender (and only a high-confidence one). Today the manager raises a well-judged alarm; next it gets the authority to actually do something about it.
