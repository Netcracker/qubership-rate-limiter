// Schedules the next request of this thread and picks its user, group, and
// operation. Both thread groups of ratelimit-load.jmx run this script. It
// sets the variables the plan reads: wait (milliseconds until the request is
// due), lag (how late the thread claimed it), op (READ, LIST, SEARCH, or STOP
// once the schedule is over), user, token, group, item, and kind.
//
// Each thread group shares one arrival schedule among its threads: Poisson
// arrivals at the rate of each step of the steps property (hotSteps for the
// hot users), given as rate:seconds pairs. A thread claims the next arrival
// and waits for it, so the rate does not depend on response times while a
// free thread is left; once every thread is busy, arrivals fall behind and
// lag grows.
//
// The tokens file (the tokens property) is read on the first request and
// read again once it changes, checked at most every five seconds, so the
// tokens that loadkit tokens -watch renews take effect.
import groovy.transform.Field

import java.util.concurrent.ThreadLocalRandom
import java.util.concurrent.atomic.AtomicLong

// The property that holds the users and tokens every thread of the run shares.
@Field static final String TOKEN_STATE = 'loadtest.tokens'

// Returns the arrival schedule the threads of one thread group share, kept in
// the property that key names. The first call makes it from the steps in the
// property that stepsProperty names.
Map schedule(String key, String stepsProperty) {
    def clock = props.get(key)
    if (clock != null) {
        return clock
    }
    synchronized (props) {
        clock = props.get(key)
        if (clock == null) {
            // Every generator of a run starts its schedule at the moment the
            // start property names, so their steps line up; without it, now.
            long start = (props.getProperty('start', '0') as long) ?: System.currentTimeMillis()
            clock = [start: start, steps: readSteps(stepsProperty), next: new AtomicLong()]
            props.put(key, clock)
        }
        return clock
    }
}

// Reads the steps in the property that stepsProperty names, rate:seconds
// pairs separated by commas, into the end of each step in microseconds since
// the start and its rate per second.
List<Map> readSteps(String stepsProperty) {
    long end = 0
    return props.getProperty(stepsProperty, '').split(',').findAll { it.trim() }.collect {
        def pair = it.trim().split(':')
        if (pair.length != 2) {
            throw new IllegalStateException("${stepsProperty} entry '${it.trim()}' is not rate:seconds")
        }
        end += (pair[1] as long) * 1000000L
        [end: end, rate: pair[0] as double]
    }
}

// Returns the users and tokens of the tokens file, read again once the file
// changes. The file is checked for a change at most every five seconds.
Map tokenState(long now) {
    def st = props.get(TOKEN_STATE)
    if (st != null && now - st.checked.get() <= 5000) {
        return st
    }
    synchronized (props) {
        st = props.get(TOKEN_STATE)
        if (st != null && now - st.checked.get() <= 5000) {
            return st
        }
        def file = new File(props.getProperty('tokens', 'tokens.csv'))
        long modified = file.lastModified()
        if (st != null && modified == st.modified) {
            st.checked.set(now)
            return st
        }
        st = readTokens(file, modified, now, st?.next ?: new AtomicLong())
        props.put(TOKEN_STATE, st)
        return st
    }
}

// Reads the tokens file that loadkit tokens writes: a header line, then a
// user,hot,token line per user, where hot is 1 for a hot user. The groups
// and mix properties are read with it.
Map readTokens(File file, long modified, long checkedAt, AtomicLong next) {
    def users = [], tokens = []
    int hot = 0
    file.eachLine('UTF-8') { line, n ->
        if (n == 1 || line.isEmpty()) {
            return
        }
        def f = line.split(',', 3)
        if (f.length < 3) {
            throw new IllegalStateException("line ${n} of ${file} is not user,hot,token")
        }
        users << f[0]
        tokens << f[2]
        if (f[1] == '1') {
            hot++
        }
    }
    if (users.isEmpty()) {
        throw new IllegalStateException("${file} holds no tokens")
    }
    def mix = props.getProperty('mix', '60,30,10').split(',').collect { it.trim() as int }
    return [modified: modified, checked: new AtomicLong(checkedAt), next: next,
            users: users as String[], tokens: tokens as String[], hot: hot,
            groups: props.getProperty('groups', 'customers,orders,incidents,billing,products').split(','),
            mix: mix, mixTotal: mix.sum()]
}

def random = ThreadLocalRandom.current()
def hotGroup = ctx.getThreadGroup().getName() == 'hot users'
def clock = hotGroup ? schedule('loadtest.clock.hot', 'hotSteps') : schedule('loadtest.clock.background', 'steps')

// The next arrival after prev, in microseconds since the start, or -1 once
// the schedule is over. A step with rate 0 sends nothing; an arrival drawn
// past the end of its step is redrawn in the next one, which keeps the
// arrivals of a piecewise constant rate a Poisson process.
def advance = { long prev ->
    long t = prev
    for (s in clock.steps) {
        if (t >= s.end) {
            continue
        }
        if (s.rate <= 0) {
            t = s.end
            continue
        }
        long gap = (long) (-Math.log(1.0d - random.nextDouble()) / s.rate * 1000000d)
        if (t + gap < s.end) {
            return t + gap
        }
        t = s.end
    }
    return -1L
}
long arrival
while (true) {
    long prev = clock.next.get()
    arrival = advance(prev)
    if (arrival < 0 || clock.next.compareAndSet(prev, arrival)) {
        break
    }
}
if (arrival < 0) {
    vars.put('op', 'STOP')
    vars.put('wait', '0')
    return
}
long due = clock.start + arrival.intdiv(1000)
long now = System.currentTimeMillis()
vars.put('wait', String.valueOf(Math.max(0L, due - now)))
vars.put('lag', String.valueOf(Math.max(0L, now - due)))

def st = tokenState(System.currentTimeMillis())
int i
String group
if (hotGroup) {
    if (st.hot == 0) {
        throw new IllegalStateException('the hot users thread group runs, but the tokens file marks no user as hot')
    }
    i = (int) (st.next.getAndIncrement() % st.hot)
    group = st.groups[i % st.groups.length]
    vars.put('kind', 'hot')
} else {
    int others = st.users.length - st.hot
    if (others == 0) {
        throw new IllegalStateException('every user of the tokens file is hot, so the background thread group has none to pick')
    }
    i = st.hot + random.nextInt(others)
    group = st.groups[random.nextInt(st.groups.length)]
    vars.put('kind', 'background')
}
int p = random.nextInt(st.mixTotal)
vars.put('op', p < st.mix[0] ? 'READ' : p < st.mix[0] + st.mix[1] ? 'LIST' : 'SEARCH')
vars.put('user', st.users[i])
vars.put('token', st.tokens[i])
vars.put('group', group)
vars.put('item', String.valueOf(random.nextInt(1000000)))
