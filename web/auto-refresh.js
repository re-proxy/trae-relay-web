'use strict';

// A single cancellable polling loop. A slow request never creates a second one.
class ConsolePoller {
  constructor({enabled, run, onError = () => {}, interval = 15000, timeout = 45000}) {
    Object.assign(this, {enabled, run, onError, interval, timeout});
    this.active = false;
    this.running = false;
    this.generation = 0;
    this.timer = null;
    this.controller = null;
  }
  start() {
    if (this.active) return;
    this.active = true;
    this.generation++;
    this.schedule(0);
  }
  stop() {
    this.active = false;
    this.generation++;
    clearTimeout(this.timer);
    this.timer = null;
    this.controller?.abort();
  }
  wake() {
    if (!this.active) this.start();
    else this.schedule(0);
  }
  schedule(delay) {
    clearTimeout(this.timer);
    if (this.active) this.timer = setTimeout(() => this.tick(), delay);
  }
  async tick() {
    this.timer = null;
    if (!this.active) return;
    if (this.running || !this.enabled()) {
      this.schedule(this.interval);
      return;
    }
    const generation = this.generation;
    const controller = new AbortController();
    this.controller = controller;
    this.running = true;
    const deadline = setTimeout(() => controller.abort(), this.timeout);
    try {
      await this.run(controller.signal);
    } catch (error) {
      if (generation === this.generation && this.active) this.onError(controller.signal.aborted ? new Error('自动更新超时') : error);
    } finally {
      clearTimeout(deadline);
      this.running = false;
      this.controller = null;
      if (this.active) this.schedule(generation === this.generation ? this.interval : 0);
    }
  }
}

// A cached response does not make the underlying balance newer. Scheduling
// from its actual timestamp avoids postponing a nearly-expired value by 60s.
function balanceCheckedAt(report, now = Date.now()) {
  const stamps = (report.accounts || []).map(account => Date.parse(account.checked_at))
    .filter(stamp => Number.isFinite(stamp) && stamp > 0 && stamp <= now);
  return stamps.length ? Math.min(...stamps) : now;
}
if (typeof module !== 'undefined' && module.exports) module.exports = {ConsolePoller, balanceCheckedAt};
