/* ============================================================
   Space Invaders – Game Engine
   Vanilla JS, Canvas 2D, Web Audio API
   ============================================================ */

(function () {
    'use strict';

    // -------------------------------------------------------
    // Constants
    // -------------------------------------------------------
    const isMobile = /Android|webOS|iPhone|iPad|iPod|BlackBerry|IEMobile|Opera Mini/i.test(navigator.userAgent) || (navigator.maxTouchPoints > 0);

    const GAME_W = 800;
    const GAME_H = 600;
    const PLAYER_W = 40;
    const PLAYER_H = 20;
    const PLAYER_SPEED = 5;
    const BULLET_SPEED = 8;
    const BULLET_W = 3;
    const BULLET_H = 12;
    const ENEMY_BULLET_SPEED = 3.5;
    const ENEMY_ROWS = 5;
    const ENEMY_COLS = 10;
    const ENEMY_W = 32;
    const ENEMY_H = 24;
    const ENEMY_PAD_X = 12;
    const ENEMY_PAD_Y = 10;
    const ENEMY_STEP_DOWN = 18;
    const POWERUP_SPEED = 1.8;
    const POWERUP_SIZE = 18;
    const POWERUP_DURATION = 8000; // ms
    const SHIELD_HITS = 3;

    // Colors per row (from bottom to top — closer = warmer)
    const ROW_COLORS = [
        '#ef4444', // red
        '#f97316', // orange
        '#f59e0b', // yellow
        '#10b981', // green
        '#06b6d4', // cyan
    ];

    const ROW_SCORES = [10, 20, 30, 40, 50];

    // Power-up types
    const PU = {
        RAPID: { color: '#f59e0b', label: 'R', desc: 'Rapid Fire' },
        MULTI: { color: '#8b5cf6', label: 'M', desc: 'Multi-Shot' },
        SHIELD: { color: '#3b82f6', label: 'S', desc: 'Shield' },
    };
    const PU_TYPES = Object.keys(PU);

    // -------------------------------------------------------
    // Stored settings
    // -------------------------------------------------------
    // localStorage throws on every access when site data is blocked; the game
    // then keeps its settings and high score until reload.
    function readStored(key) {
        try {
            return localStorage.getItem(key);
        } catch (_) {
            return null;
        }
    }

    function writeStored(key, value) {
        try {
            localStorage.setItem(key, value);
        } catch (_) { /* storage unavailable: the value lasts until reload */ }
    }

    // -------------------------------------------------------
    // Graphics settings
    // -------------------------------------------------------
    // Every option is a string enum persisted in localStorage.
    const GFX_OPTIONS = {
        resolution: ['retro', 'sharp'],
        glow: ['off', 'on'],
        particles: ['off', 'low', 'high'],
        stars: ['off', 'low', 'high'],
        scanlines: ['off', 'on'],
        fps: ['off', 'on'],
    };

    // Presets leave the FPS counter alone. Low is the original mobile look
    // and medium the original desktop look; high adds HiDPI rendering.
    const GFX_PRESETS = {
        low: { resolution: 'retro', glow: 'off', particles: 'low', stars: 'low', scanlines: 'off' },
        medium: { resolution: 'retro', glow: 'on', particles: 'high', stars: 'high', scanlines: 'on' },
        high: { resolution: 'sharp', glow: 'on', particles: 'high', stars: 'high', scanlines: 'on' },
    };

    const STAR_COUNTS = { off: 0, low: 50, high: 120 };
    const PARTICLE_LIMITS = { off: 0, low: 30, high: 150 };
    const MAX_RENDER_SCALE = 3;

    function loadGfx() {
        const settings = Object.assign({ fps: 'off' }, GFX_PRESETS[isMobile ? 'low' : 'medium']);
        let saved = null;
        try {
            saved = JSON.parse(readStored('kula_invaders_gfx') || 'null');
        } catch (_) { /* corrupt stored value: keep the defaults */ }
        if (saved && typeof saved === 'object') {
            for (const key of Object.keys(GFX_OPTIONS)) {
                if (GFX_OPTIONS[key].includes(saved[key])) settings[key] = saved[key];
            }
        }
        return settings;
    }

    const gfx = loadGfx();

    // -------------------------------------------------------
    // Audio (Web Audio API — synthesized)
    // -------------------------------------------------------
    let audioCtx = null;
    let isMuted = readStored('kula_invaders_muted') === 'true';

    function ensureAudio() {
        if (!audioCtx) audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    }

    function playTone(freq, duration, type, vol) {
        if (isMuted) return;
        try {
            ensureAudio();
            const osc = audioCtx.createOscillator();
            const gain = audioCtx.createGain();
            osc.type = type || 'square';
            osc.frequency.value = freq;
            gain.gain.setValueAtTime(vol || 0.08, audioCtx.currentTime);
            gain.gain.exponentialRampToValueAtTime(0.001, audioCtx.currentTime + duration);
            osc.connect(gain);
            gain.connect(audioCtx.destination);
            osc.start();
            osc.stop(audioCtx.currentTime + duration);
        } catch (_) { /* audio not available */ }
    }

    const SFX = {
        shoot: () => playTone(880, 0.08, 'square', 0.06),
        hit: () => { playTone(220, 0.15, 'sawtooth', 0.08); playTone(110, 0.2, 'square', 0.06); },
        explode: () => { playTone(80, 0.3, 'sawtooth', 0.1); playTone(40, 0.4, 'triangle', 0.08); },
        powerup: () => { playTone(523, 0.1, 'sine', 0.07); playTone(659, 0.1, 'sine', 0.07); playTone(784, 0.15, 'sine', 0.09); },
        playerHit: () => { playTone(150, 0.3, 'sawtooth', 0.12); playTone(60, 0.5, 'square', 0.1); },
        levelUp: () => { [523, 659, 784, 1047].forEach((f, i) => setTimeout(() => playTone(f, 0.15, 'sine', 0.08), i * 100)); },
        newHighScore: () => {
            setTimeout(() => {
                [440, 554.37, 659.25, 880].forEach((f, i) => setTimeout(() => playTone(f, 0.15, 'square', 0.1), i * 150));
                setTimeout(() => playTone(880, 0.4, 'square', 0.1), 600);
            }, 800);
        },
    };

    // -------------------------------------------------------
    // DOM
    // -------------------------------------------------------
    const canvas = document.getElementById('game-canvas');
    const ctx = canvas.getContext('2d');
    const $startScreen = document.getElementById('start-screen');
    const $pauseScreen = document.getElementById('pause-screen');
    const $gameoverScreen = document.getElementById('gameover-screen');
    const $levelupScreen = document.getElementById('levelup-screen');
    const $hudScore = document.getElementById('hud-score');
    const $hudLevel = document.getElementById('hud-level');
    const $hudHigh = document.getElementById('hud-high');
    const $hudLives = document.getElementById('hud-lives');
    const $finalScore = document.getElementById('final-score');
    const $finalHigh = document.getElementById('final-high');
    const $levelupNum = document.getElementById('levelup-num');
    const $newHighScoreAlert = document.getElementById('new-highscore-alert');
    const $btnLeft = document.getElementById('btn-left');
    const $btnRight = document.getElementById('btn-right');
    const $btnFire = document.getElementById('btn-fire');
    const $btnPause = document.getElementById('btn-pause');
    const $mobileControls = document.getElementById('mobile-controls');
    const $btnFullscreen = document.getElementById('btn-fullscreen');
    const $btnMute = document.getElementById('btn-mute');
    const $muteIcon = document.getElementById('mute-icon');
    const $btnSettings = document.getElementById('btn-settings');
    const $settingsScreen = document.getElementById('settings-screen');
    const $btnSettingsClose = document.getElementById('btn-settings-close');
    const $btnResume = document.getElementById('btn-resume');
    const $btnRestart = document.getElementById('btn-restart');
    const $btnPauseSettings = document.getElementById('btn-pause-settings');

    // -------------------------------------------------------
    // State
    // -------------------------------------------------------
    let state = 'start'; // start | playing | paused | gameover | levelup
    let score = 0;
    let level = 1;
    let lives = 3;
    let highScore = parseInt(readStored('kula_invaders_high') || '0', 10);
    let shootCooldown = 0;
    let lastEscPress = 0;
    let levelupTimer = 0;
    let settingsOpen = false;

    // Input
    const keys = {};

    // Stars (parallax background)
    let stars = [];

    // Player
    let player = {};

    // Enemies
    let enemies = [];
    let enemyDir = 1;
    let enemySpeed = 0;
    let enemyShootTimer = 0;

    // Bullets
    let playerBullets = [];
    let enemyBullets = [];

    // Particles
    let particles = [];

    // Power-ups
    let powerups = [];
    let activePowerups = {}; // type -> expiry timestamp

    // Shield
    let shieldHP = 0;

    // -------------------------------------------------------
    // Scaling
    // -------------------------------------------------------
    let scale = 1;
    let renderScale = 1; // canvas pixels per game unit
    function resize() {
        const isFS = !!document.fullscreenElement;
        const maxW = window.innerWidth * (isFS ? 0.98 : 0.92);
        const maxH = window.innerHeight * (isFS ? 0.98 : 0.88);
        scale = Math.min(maxW / GAME_W, maxH / GAME_H, isFS ? 2.5 : 1.2);
        // Retro keeps the 800x600 backing store and lets CSS upscale it;
        // sharp renders at the displayed size in device pixels.
        renderScale = gfx.resolution === 'sharp'
            ? Math.min(MAX_RENDER_SCALE, Math.max(1, scale * (window.devicePixelRatio || 1)))
            : 1;
        canvas.width = Math.round(GAME_W * renderScale);
        canvas.height = Math.round(GAME_H * renderScale);
        canvas.style.width = (GAME_W * scale) + 'px';
        canvas.style.height = (GAME_H * scale) + 'px';
    }
    window.addEventListener('resize', resize);
    document.addEventListener('fullscreenchange', resize);
    resize();

    // Neon glow through canvas shadows. shadowBlur ignores the transform, so
    // it is scaled by hand to look the same at every resolution.
    function setGlow(color, blur) {
        if (gfx.glow !== 'on') return;
        ctx.shadowColor = color;
        ctx.shadowBlur = blur * renderScale;
    }

    // -------------------------------------------------------
    // Stars
    // -------------------------------------------------------
    function initStars() {
        stars = [];
        for (let i = 0; i < STAR_COUNTS[gfx.stars]; i++) {
            stars.push({
                x: Math.random() * GAME_W,
                y: Math.random() * GAME_H,
                size: Math.random() * 1.5 + 0.5,
                speed: Math.random() * 0.4 + 0.1,
                brightness: Math.random() * 0.5 + 0.3,
            });
        }
    }

    function updateStars() {
        const time = Date.now() * 0.002;
        for (const s of stars) {
            s.y += s.speed;
            if (s.y > GAME_H) { s.y = 0; s.x = Math.random() * GAME_W; }
            s.brightness = 0.3 + Math.sin(time + s.x) * 0.2;
        }
    }

    function drawStars() {
        for (const s of stars) {
            ctx.fillStyle = `rgba(148, 163, 184, ${s.brightness})`;
            ctx.fillRect(s.x, s.y, s.size, s.size);
        }
    }

    // -------------------------------------------------------
    // Player
    // -------------------------------------------------------
    function initPlayer() {
        player = {
            x: GAME_W / 2 - PLAYER_W / 2,
            y: GAME_H - 50,
            w: PLAYER_W,
            h: PLAYER_H,
            invincible: 0, // frames of invincibility after getting hit
        };
        shieldHP = 0;
        activePowerups = {};
    }

    function updatePlayer() {
        if (keys['ArrowLeft'] || keys['a']) player.x -= PLAYER_SPEED;
        if (keys['ArrowRight'] || keys['d']) player.x += PLAYER_SPEED;
        player.x = Math.max(0, Math.min(GAME_W - player.w, player.x));

        if (player.invincible > 0) player.invincible--;

        if (shootCooldown > 0) shootCooldown--;

        const cooldownMax = activePowerups.RAPID ? 5 : 15;
        const wantsToShoot = keys[' '] || keys['ArrowUp'] || keys['w'];

        if (wantsToShoot && shootCooldown <= 0) {
            shootCooldown = cooldownMax;
            SFX.shoot();

            if (activePowerups.MULTI) {
                // Three-way shot
                playerBullets.push({ x: player.x + player.w / 2 - BULLET_W / 2, y: player.y - BULLET_H, dx: 0 });
                playerBullets.push({ x: player.x + player.w / 2 - BULLET_W / 2, y: player.y - BULLET_H, dx: -2 });
                playerBullets.push({ x: player.x + player.w / 2 - BULLET_W / 2, y: player.y - BULLET_H, dx: 2 });
            } else {
                playerBullets.push({ x: player.x + player.w / 2 - BULLET_W / 2, y: player.y - BULLET_H, dx: 0 });
            }
        }

        // Check power-up expirations
        const now = Date.now();
        for (const key of Object.keys(activePowerups)) {
            if (activePowerups[key] && now > activePowerups[key]) {
                delete activePowerups[key];
                if (key === 'SHIELD') shieldHP = 0;
            }
        }
    }

    function drawPlayer() {
        if (player.invincible > 0 && Math.floor(player.invincible / 3) % 2 === 0) return;

        const cx = player.x + player.w / 2;
        const cy = player.y + player.h / 2;

        // Ship body
        ctx.fillStyle = '#3b82f6';
        setGlow('#3b82f6', 12);
        ctx.beginPath();
        ctx.moveTo(cx, player.y - 4);
        ctx.lineTo(player.x + player.w + 2, player.y + player.h);
        ctx.lineTo(player.x - 2, player.y + player.h);
        ctx.closePath();
        ctx.fill();

        // Ship accent
        ctx.fillStyle = '#60a5fa';
        ctx.beginPath();
        ctx.moveTo(cx, player.y);
        ctx.lineTo(cx + 6, player.y + player.h - 4);
        ctx.lineTo(cx - 6, player.y + player.h - 4);
        ctx.closePath();
        ctx.fill();

        // Engine glow
        ctx.fillStyle = `rgba(6, 182, 212, ${0.5 + Math.sin(Date.now() * 0.01) * 0.3})`;
        setGlow('#06b6d4', 8);
        ctx.fillRect(cx - 4, player.y + player.h, 8, 4 + Math.sin(Date.now() * 0.02) * 2);
        ctx.shadowBlur = 0;

        // Shield bubble
        if (shieldHP > 0) {
            const alpha = 0.15 + shieldHP * 0.08;
            ctx.strokeStyle = `rgba(59, 130, 246, ${alpha})`;
            setGlow('#3b82f6', 10);
            ctx.lineWidth = 2;
            ctx.beginPath();
            ctx.arc(cx, cy + 2, 28, 0, Math.PI * 2);
            ctx.stroke();
            ctx.shadowBlur = 0;
            ctx.lineWidth = 1;
        }
    }

    // -------------------------------------------------------
    // Enemies
    // -------------------------------------------------------
    function initEnemies() {
        enemies = [];
        enemyDir = 1;
        const startX = (GAME_W - (ENEMY_COLS * (ENEMY_W + ENEMY_PAD_X))) / 2;
        const startY = 60;
        for (let r = 0; r < ENEMY_ROWS; r++) {
            for (let c = 0; c < ENEMY_COLS; c++) {
                enemies.push({
                    x: startX + c * (ENEMY_W + ENEMY_PAD_X),
                    y: startY + r * (ENEMY_H + ENEMY_PAD_Y),
                    w: ENEMY_W,
                    h: ENEMY_H,
                    row: r,
                    alive: true,
                    frame: 0,
                });
            }
        }
        const aliveCount = enemies.filter(e => e.alive).length;
        enemySpeed = 0.4 + level * 0.15 + (1 - aliveCount / (ENEMY_ROWS * ENEMY_COLS)) * 2;
        enemyShootTimer = 0;
    }

    function updateEnemies() {
        let aliveCount = 0;
        let leftMost = GAME_W, rightMost = 0;

        for (let i = 0; i < enemies.length; i++) {
            const e = enemies[i];
            if (!e.alive) continue;
            aliveCount++;
            if (e.x < leftMost) leftMost = e.x;
            if (e.x + e.w > rightMost) rightMost = e.x + e.w;
        }

        if (aliveCount === 0) return;
        enemySpeed = 0.4 + level * 0.15 + (1 - aliveCount / (ENEMY_ROWS * ENEMY_COLS)) * 2.5;

        // Move enemies
        let hitEdge = false;
        if (enemyDir === 1 && rightMost > GAME_W - 5) hitEdge = true;
        else if (enemyDir === -1 && leftMost < 5) hitEdge = true;

        if (hitEdge) {
            enemyDir *= -1;
            for (let i = 0; i < enemies.length; i++) {
                const e = enemies[i];
                if (!e.alive) continue;
                e.y += ENEMY_STEP_DOWN;
                if (e.y + e.h >= player.y - 10) {
                    gameOver();
                    return;
                }
            }
        } else {
            const shift = enemyDir * enemySpeed;
            for (let i = 0; i < enemies.length; i++) {
                const e = enemies[i];
                if (!e.alive) continue;
                e.x += shift;
            }
        }

        // Enemy shooting
        enemyShootTimer++;
        const shootInterval = Math.max(20, 60 - level * 5);
        if (enemyShootTimer >= shootInterval) {
            enemyShootTimer = 0;
            // Pick a random alive enemy column
            const cols = {};
            for (let i = 0; i < enemies.length; i++) {
                const e = enemies[i];
                if (!e.alive) continue;
                const cKey = Math.round(e.x / (ENEMY_W + ENEMY_PAD_X));
                if (!cols[cKey] || e.y > cols[cKey].y) cols[cKey] = e;
            }
            const bottoms = Object.values(cols);
            if (bottoms.length > 0) {
                const shooter = bottoms[Math.floor(Math.random() * bottoms.length)];
                enemyBullets.push({
                    x: shooter.x + shooter.w / 2,
                    y: shooter.y + shooter.h,
                    color: ROW_COLORS[shooter.row] || '#ef4444',
                });
            }
        }

        // Animate frames (only every ~0.5s)
        if (Math.floor(Date.now() / 500) % 2 === 0) {
            for (let i = 0; i < enemies.length; i++) enemies[i].frame = 0;
        } else {
            for (let i = 0; i < enemies.length; i++) enemies[i].frame = 1;
        }
    }

    function drawEnemies() {
        for (const e of enemies) {
            if (!e.alive) continue;
            const color = ROW_COLORS[e.row] || '#ef4444';
            ctx.fillStyle = color;
            setGlow(color, 6);

            const cx = e.x + e.w / 2;
            const cy = e.y + e.h / 2;

            // Body
            ctx.fillRect(e.x + 4, e.y + 4, e.w - 8, e.h - 8);
            // Eyes
            ctx.fillStyle = '#0a0e17';
            ctx.fillRect(cx - 7, cy - 4, 4, 4);
            ctx.fillRect(cx + 3, cy - 4, 4, 4);
            // Antenna/legs depending on frame
            ctx.fillStyle = color;
            if (e.frame === 0) {
                ctx.fillRect(e.x, e.y + e.h - 6, 4, 6);
                ctx.fillRect(e.x + e.w - 4, e.y + e.h - 6, 4, 6);
            } else {
                ctx.fillRect(e.x + 2, e.y + e.h - 4, 4, 4);
                ctx.fillRect(e.x + e.w - 6, e.y + e.h - 4, 4, 4);
            }
            // Top antennae
            ctx.fillRect(cx - 8, e.y, 2, 5);
            ctx.fillRect(cx + 6, e.y, 2, 5);

            ctx.shadowBlur = 0;
        }
    }

    // -------------------------------------------------------
    // Bullets
    // -------------------------------------------------------
    function updateBullets() {
        // Player bullets
        for (let i = playerBullets.length - 1; i >= 0; i--) {
            const b = playerBullets[i];
            b.y -= BULLET_SPEED;
            b.x += (b.dx || 0);
            if (b.y + BULLET_H < 0 || b.x < 0 || b.x > GAME_W) {
                playerBullets.splice(i, 1);
                continue;
            }
            // Check hit enemy
            for (const e of enemies) {
                if (!e.alive) continue;
                if (b.x < e.x + e.w && b.x + BULLET_W > e.x && b.y < e.y + e.h && b.y + BULLET_H > e.y) {
                    e.alive = false;
                    playerBullets.splice(i, 1);
                    score += ROW_SCORES[e.row] || 10;
                    SFX.hit();
                    spawnExplosion(e.x + e.w / 2, e.y + e.h / 2, ROW_COLORS[e.row]);
                    
                    // Check level complete immediately
                    let anyAlive = false;
                    for (let j = 0; j < enemies.length; j++) {
                        if (enemies[j].alive) { anyAlive = true; break; }
                    }
                    if (!anyAlive) nextLevel();

                    // Chance to drop power-up
                    if (Math.random() < 0.08) {
                        spawnPowerup(e.x + e.w / 2, e.y + e.h / 2);
                    }
                    break;
                }
            }
        }

        // Enemy bullets
        for (let i = enemyBullets.length - 1; i >= 0; i--) {
            const b = enemyBullets[i];
            b.y += ENEMY_BULLET_SPEED + level * 0.2;
            if (b.y > GAME_H) {
                enemyBullets.splice(i, 1);
                continue;
            }
            // Check hit player
            if (player.invincible <= 0 &&
                b.x > player.x && b.x < player.x + player.w &&
                b.y > player.y && b.y < player.y + player.h) {
                enemyBullets.splice(i, 1);
                playerHit();
            }
        }
    }

    function drawBullets() {
        // Player bullets — cyan glow
        ctx.fillStyle = '#06b6d4';
        setGlow('#06b6d4', 8);
        for (const b of playerBullets) {
            ctx.fillRect(b.x, b.y, BULLET_W, BULLET_H);
            // Trail
            ctx.fillStyle = 'rgba(6, 182, 212, 0.3)';
            ctx.fillRect(b.x - 1, b.y + BULLET_H, BULLET_W + 2, 6);
            ctx.fillStyle = '#06b6d4';
        }

        // Enemy bullets — colored glow
        for (const b of enemyBullets) {
            ctx.fillStyle = b.color || '#ef4444';
            setGlow(b.color || '#ef4444', 6);
            ctx.beginPath();
            ctx.arc(b.x, b.y, 3, 0, Math.PI * 2);
            ctx.fill();
            // Trail
            ctx.fillStyle = (b.color || '#ef4444') + '44';
            ctx.beginPath();
            ctx.arc(b.x, b.y - 5, 2, 0, Math.PI * 2);
            ctx.fill();
        }
        ctx.shadowBlur = 0;
    }

    // -------------------------------------------------------
    // Particles
    // -------------------------------------------------------
    function spawnExplosion(x, y, color) {
        if (particles.length >= PARTICLE_LIMITS[gfx.particles]) return;
        const count = 15 + Math.floor(Math.random() * 10);
        for (let i = 0; i < count; i++) {
            const angle = Math.random() * Math.PI * 2;
            const speed = Math.random() * 3 + 1;
            particles.push({
                x, y,
                vx: Math.cos(angle) * speed,
                vy: Math.sin(angle) * speed,
                life: 1,
                decay: 0.015 + Math.random() * 0.02,
                size: Math.random() * 3 + 1,
                color: color || '#f59e0b',
            });
        }
    }

    function updateParticles() {
        for (let i = particles.length - 1; i >= 0; i--) {
            const p = particles[i];
            p.x += p.vx;
            p.y += p.vy;
            p.vy += 0.03; // slight gravity
            p.life -= p.decay;
            if (p.life <= 0) particles.splice(i, 1);
        }
    }

    function drawParticles() {
        for (const p of particles) {
            ctx.globalAlpha = p.life;
            ctx.fillStyle = p.color;
            setGlow(p.color, 4);
            ctx.fillRect(p.x - p.size / 2, p.y - p.size / 2, p.size, p.size);
        }
        ctx.globalAlpha = 1;
        ctx.shadowBlur = 0;
    }

    // -------------------------------------------------------
    // Power-ups
    // -------------------------------------------------------
    function spawnPowerup(x, y) {
        const type = PU_TYPES[Math.floor(Math.random() * PU_TYPES.length)];
        powerups.push({ x, y, type, time: 0 });
    }

    function updatePowerups() {
        for (let i = powerups.length - 1; i >= 0; i--) {
            const pu = powerups[i];
            pu.y += POWERUP_SPEED;
            pu.time++;
            if (pu.y > GAME_H) {
                powerups.splice(i, 1);
                continue;
            }
            // Collect
            const dx = (pu.x) - (player.x + player.w / 2);
            const dy = (pu.y) - (player.y + player.h / 2);
            if (Math.abs(dx) < 22 && Math.abs(dy) < 22) {
                applyPowerup(pu.type);
                powerups.splice(i, 1);
                SFX.powerup();
            }
        }
    }

    function applyPowerup(type) {
        activePowerups[type] = Date.now() + POWERUP_DURATION;
        if (type === 'SHIELD') {
            shieldHP = SHIELD_HITS;
        }
    }

    function drawPowerups() {
        for (const pu of powerups) {
            const info = PU[pu.type];
            const wobble = Math.sin(pu.time * 0.1) * 2;
            ctx.save();
            ctx.translate(pu.x, pu.y + wobble);

            // Outer glow
            ctx.fillStyle = info.color + '33';
            setGlow(info.color, 12);
            ctx.beginPath();
            ctx.arc(0, 0, POWERUP_SIZE, 0, Math.PI * 2);
            ctx.fill();

            // Inner
            ctx.fillStyle = info.color;
            ctx.beginPath();
            ctx.arc(0, 0, POWERUP_SIZE * 0.6, 0, Math.PI * 2);
            ctx.fill();

            // Label
            ctx.fillStyle = '#0a0e17';
            ctx.font = 'bold 12px "JetBrains Mono"';
            ctx.textAlign = 'center';
            ctx.textBaseline = 'middle';
            ctx.fillText(info.label, 0, 1);

            ctx.shadowBlur = 0;
            ctx.restore();
        }
    }

    // -------------------------------------------------------
    // Active power-up indicators
    // -------------------------------------------------------
    function drawActivePowerups() {
        const now = Date.now();
        let idx = 0;
        for (const key of Object.keys(activePowerups)) {
            const remaining = activePowerups[key] - now;
            if (remaining <= 0) continue;
            const info = PU[key];
            const pct = remaining / POWERUP_DURATION;
            const bx = 10;
            const by = GAME_H - 30 - idx * 22;

            // Bar background
            ctx.fillStyle = 'rgba(17, 24, 39, 0.7)';
            ctx.fillRect(bx, by, 100, 16);

            // Bar fill
            ctx.fillStyle = info.color + 'aa';
            ctx.fillRect(bx, by, 100 * pct, 16);

            // Border
            ctx.strokeStyle = info.color;
            ctx.lineWidth = 1;
            ctx.strokeRect(bx, by, 100, 16);

            // Label
            ctx.fillStyle = '#f1f5f9';
            ctx.font = '10px "JetBrains Mono"';
            ctx.textAlign = 'left';
            ctx.textBaseline = 'middle';
            ctx.fillText(info.desc, bx + 4, by + 8);

            idx++;
        }
    }

    // -------------------------------------------------------
    // Player hit
    // -------------------------------------------------------
    function playerHit() {
        if (shieldHP > 0) {
            shieldHP--;
            SFX.hit();
            spawnExplosion(player.x + player.w / 2, player.y, '#3b82f6');
            if (shieldHP <= 0) delete activePowerups.SHIELD;
            return;
        }
        lives--;
        SFX.playerHit();
        spawnExplosion(player.x + player.w / 2, player.y + player.h / 2, '#3b82f6');
        if (lives <= 0) {
            gameOver();
        } else {
            player.invincible = 90; // ~1.5 seconds
        }
        updateHUD();
    }

    // -------------------------------------------------------
    // Game flow
    // -------------------------------------------------------
    // saveHighScore stores the current score if it beats the saved record (or,
    // without storage, this page's best) and reports whether it did.
    function saveHighScore() {
        const previousHigh = parseInt(readStored('kula_invaders_high') ?? String(highScore), 10) || 0;
        if (score > previousHigh && score > 0) {
            highScore = score;
            writeStored('kula_invaders_high', String(highScore));
            return true;
        }
        return false;
    }

    function gameOver() {
        state = 'gameover';

        const isNewHighScore = saveHighScore();

        $finalScore.textContent = score;
        $finalHigh.textContent = highScore;

        if (isNewHighScore) {
            if ($newHighScoreAlert) $newHighScoreAlert.classList.remove('hidden');
            SFX.newHighScore();
        } else {
            if ($newHighScoreAlert) $newHighScoreAlert.classList.add('hidden');
        }

        $gameoverScreen.classList.remove('hidden');
        updateMobileControlsVisibility();
        SFX.explode();
        canvas.style.cursor = 'default';

        // Score endpoints are public, browser-originated APIs. Do not attach
        // Kula session cookies, follow redirects, or disclose the dashboard URL.
        const scoreUrl = canvas.getAttribute('data-score-url');
        if (scoreUrl) {
            fetch(scoreUrl, {
                method: 'POST',
                credentials: 'omit',
                redirect: 'error',
                referrerPolicy: 'no-referrer',
                headers: {
                    'Content-Type': 'application/json'
                },
                body: JSON.stringify({ score: score })
            }).catch(() => {});
        }
    }

    function nextLevel() {
        level++;
        state = 'levelup';
        $levelupNum.textContent = level;
        $levelupScreen.classList.remove('hidden');
        SFX.levelUp();
        canvas.style.cursor = 'default';
        clearTimeout(levelupTimer);
        levelupTimer = setTimeout(() => {
            $levelupScreen.classList.add('hidden');
            initEnemies();
            playerBullets = [];
            enemyBullets = [];
            powerups = [];
            if (settingsOpen) {
                // Settings were opened during the banner: hold the new level paused.
                state = 'paused';
                $pauseScreen.classList.remove('hidden');
            } else {
                state = 'playing';
                canvas.style.cursor = 'none';
            }
            updateMobileControlsVisibility();
        }, 2000);
    }

    // restartGame abandons a paused run, or skips the game over screen. An
    // abandoned run still counts toward the local high score but is not
    // submitted to the score endpoint.
    function restartGame() {
        if (state !== 'paused' && state !== 'gameover') return;
        if (state === 'paused') saveHighScore();
        startGame();
    }

    function startGame() {
        ensureAudio();
        clearTimeout(levelupTimer);
        releaseFocus();
        score = 0;
        level = 1;
        lives = 3;
        playerBullets = [];
        enemyBullets = [];
        particles = [];
        powerups = [];
        activePowerups = {};
        shieldHP = 0;
        shootCooldown = 0;
        initPlayer();
        initEnemies();
        updateHUD(true);
        $startScreen.classList.add('hidden');
        $gameoverScreen.classList.add('hidden');
        $pauseScreen.classList.add('hidden');
        $levelupScreen.classList.add('hidden');
        state = 'playing';
        updateMobileControlsVisibility();
        canvas.style.cursor = 'none';

        if (isMobile) {
            tryEnterImmersiveMode();
        }
    }

    async function tryEnterImmersiveMode() {
        try {
            if (!document.fullscreenElement) {
                await document.documentElement.requestFullscreen();
            }
            if (screen.orientation && screen.orientation.lock) {
                await screen.orientation.lock('landscape').catch(() => {});
            }
        } catch (e) {
            console.warn("Immersive mode failed:", e);
        }
    }

    // -------------------------------------------------------
    // HUD
    // -------------------------------------------------------
    let lastHUD = { score: -1, level: -1, high: -1, lives: '' };
    function updateHUD(force = false) {
        if (state !== 'playing' && state !== 'paused' && !force) return;

        if (force || score !== lastHUD.score) {
            $hudScore.textContent = score;
            lastHUD.score = score;
        }
        if (force || level !== lastHUD.level) {
            $hudLevel.textContent = level;
            lastHUD.level = level;
        }
        if (force || highScore !== lastHUD.high) {
            $hudHigh.textContent = highScore;
            lastHUD.high = highScore;
        }
        const livesStr = '♥'.repeat(Math.max(0, lives));
        if (force || livesStr !== lastHUD.lives) {
            $hudLives.textContent = livesStr;
            lastHUD.lives = livesStr;
        }
    }

    // -------------------------------------------------------
    // Game loop
    // -------------------------------------------------------
    function update() {
        if (state !== 'playing') return;

        updateStars();
        updatePlayer();
        updateEnemies();
        updateBullets();
        updateParticles();
        updatePowerups();
        updateHUD();
    }
    function draw() {
        // Resizing the canvas resets its state, so the scale is set per frame.
        ctx.setTransform(renderScale, 0, 0, renderScale, 0, 0);
        ctx.clearRect(0, 0, GAME_W, GAME_H);

        // Background gradient
        const grad = ctx.createLinearGradient(0, 0, 0, GAME_H);
        grad.addColorStop(0, '#0a0e17');
        grad.addColorStop(1, '#111827');
        ctx.fillStyle = grad;
        ctx.fillRect(0, 0, GAME_W, GAME_H);

        drawStars();

        if (state === 'playing' || state === 'paused' || state === 'levelup') {
            drawEnemies();
            drawPlayer();
            drawBullets();
            drawParticles();
            drawPowerups();
            drawActivePowerups();
        }

        // Ground line
        ctx.strokeStyle = 'rgba(59, 130, 246, 0.15)';
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(0, GAME_H - 20);
        ctx.lineTo(GAME_W, GAME_H - 20);
        ctx.stroke();

        if (gfx.scanlines === 'on') drawScanlines();
        if (gfx.fps === 'on') drawFPS();
    }

    // CRT scanlines: one dark line every 3 canvas pixels. They are painted
    // into the canvas because a full-page CSS overlay above the constantly
    // redrawn canvas made the browser re-composite the whole page each frame,
    // which dropped software-rendered Firefox from 60 to about 14 fps.
    let scanlinePattern = null;
    function drawScanlines() {
        if (!scanlinePattern) {
            const tile = document.createElement('canvas');
            tile.width = 1;
            tile.height = 3;
            const tileCtx = tile.getContext('2d');
            tileCtx.fillStyle = 'rgba(0, 0, 0, 0.03)';
            tileCtx.fillRect(0, 0, 1, 1);
            scanlinePattern = ctx.createPattern(tile, 'repeat');
        }
        ctx.save();
        ctx.setTransform(1, 0, 0, 1, 0, 0);
        ctx.fillStyle = scanlinePattern;
        ctx.fillRect(0, 0, canvas.width, canvas.height);
        ctx.restore();
    }

    // -------------------------------------------------------
    // FPS counter
    // -------------------------------------------------------
    // Frames per second over the last second, plus the longest frame in it:
    // an average hides a single stall that the maximum shows.
    let fps = 0;
    let worstFrameMs = 0;
    let fpsFrames = 0;
    let fpsSince = performance.now();
    let fpsLastFrame = fpsSince;
    let fpsWorst = 0;

    function tickFPS(now) {
        fpsWorst = Math.max(fpsWorst, now - fpsLastFrame);
        fpsLastFrame = now;
        fpsFrames++;
        if (now - fpsSince >= 1000) {
            fps = Math.round(fpsFrames * 1000 / (now - fpsSince));
            worstFrameMs = Math.round(fpsWorst);
            fpsFrames = 0;
            fpsWorst = 0;
            fpsSince = now;
        }
    }

    function drawFPS() {
        ctx.fillStyle = 'rgba(148, 163, 184, 0.8)';
        ctx.font = '8px "Press Start 2P", monospace';
        ctx.textAlign = 'right';
        ctx.textBaseline = 'bottom';
        ctx.fillText(fps + ' FPS  MAX ' + worstFrameMs + 'MS', GAME_W - 8, GAME_H - 6);
    }

    // The game advances in fixed 60 Hz ticks whatever the display does: a
    // slow frame runs the ticks it missed instead of slowing the game down,
    // and a 120/144 Hz display does not speed it up.
    const TICK_MS = 1000 / 60;
    const MAX_CATCHUP_MS = 100; // longer stalls, such as a hidden tab, are dropped
    let lastFrameTime = performance.now();
    let tickDebt = 0;

    function loop(now) {
        tickFPS(now);
        const elapsed = Math.min(Math.max(0, now - lastFrameTime), MAX_CATCHUP_MS);
        lastFrameTime = now;
        if (state === 'playing') {
            tickDebt += elapsed;
            // Frame timestamps jitter around the refresh interval; the 10%
            // slack keeps a 60 Hz display at exactly one tick per frame, and
            // the debt carried over keeps the average rate exact.
            while (state === 'playing' && tickDebt >= TICK_MS * 0.9) {
                update();
                tickDebt -= TICK_MS;
            }
        } else {
            tickDebt = 0;
        }
        draw();
        requestAnimationFrame(loop);
    }

    // -------------------------------------------------------
    // Input
    // -------------------------------------------------------
    window.addEventListener('keydown', (e) => {
        const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
        const shortcut = !e.repeat && !e.ctrlKey && !e.metaKey && !e.altKey;

        // The settings panel behaves as a dialog: Tab, Enter and Space
        // operate its buttons and the game ignores every other key.
        if (settingsOpen) {
            if (key === 'Escape' || (key === 'g' && shortcut)) {
                e.preventDefault();
                closeSettings();
            }
            return;
        }

        keys[e.key] = true;

        if (e.key === 'Enter') {
            if (state === 'start' || state === 'gameover') {
                // Keep Enter from also pressing a button that still has focus.
                e.preventDefault();
                startGame();
            }
        }

        if (shortcut && key === 'r') restartGame();
        if (shortcut && key === 'g') openSettings();

        if (e.key === 'Escape') {
            const now = Date.now();
            const isInFS = !!document.fullscreenElement;
            
            if (isInFS) {
                e.preventDefault(); // Prevent browser default FS exit
                if (now - lastEscPress < 500) {
                    if (document.exitFullscreen) document.exitFullscreen();
                } else {
                    togglePause();
                }
            } else {
                togglePause();
            }
            lastEscPress = now;
        }

        // Prevent scrolling with space/arrows
        if (['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', ' '].includes(e.key)) {
            e.preventDefault();
        }
    });

    window.addEventListener('keyup', (e) => {
        keys[e.key] = false;
    });

    // Mobile / Touch controls
    function setupMobileControls() {
        const handleBtn = (btn, key) => {
            if (!btn) return;
            btn.addEventListener('pointerdown', (e) => {
                e.preventDefault();
                keys[key] = true;
            });
            const release = (e) => {
                e.preventDefault();
                keys[key] = false;
            };
            btn.addEventListener('pointerup', release);
            btn.addEventListener('pointerleave', release);
            btn.addEventListener('pointercancel', release);
        };

        handleBtn($btnLeft, 'ArrowLeft');
        handleBtn($btnRight, 'ArrowRight');
        handleBtn($btnFire, ' ');

        if ($btnPause) {
            $btnPause.addEventListener('click', (e) => {
                e.preventDefault();
                togglePause();
            });
        }

        // Global tap to start/resume/restart
        window.addEventListener('pointerdown', (e) => {
            if (isMobile) tryEnterImmersiveMode();
            
            // Only handle global taps if not clicking a button or the settings
            if (settingsOpen || e.target.closest('.mobile-btn, .hud-controls, .menu-btn')) return;

            if (state === 'start' || state === 'gameover') {
                startGame();
            } else if (state === 'paused') {
                togglePause();
            }
        });

        // Fullscreen Toggle
        if ($btnFullscreen) {
            $btnFullscreen.addEventListener('click', (e) => {
                e.preventDefault();
                toggleFullscreen();
            });
        }

        // Mute Toggle
        if ($btnMute) {
            updateMuteIcon();
            $btnMute.addEventListener('click', (e) => {
                e.preventDefault();
                toggleMute();
            });
        }

        // Graphics settings
        $btnSettings.addEventListener('click', (e) => {
            e.preventDefault();
            if (settingsOpen) closeSettings();
            else openSettings();
        });
        $btnSettingsClose.addEventListener('click', closeSettings);
        $settingsScreen.addEventListener('click', (e) => {
            const btn = e.target.closest('.opt-btn');
            if (btn) chooseGfxOption(btn.closest('.settings-options').dataset.setting, btn.dataset.value);
        });

        // Pause menu
        $btnResume.addEventListener('click', () => {
            if (state === 'paused') togglePause();
        });
        $btnRestart.addEventListener('click', restartGame);
        $btnPauseSettings.addEventListener('click', openSettings);
    }

    // -------------------------------------------------------
    // Graphics settings panel
    // -------------------------------------------------------
    function chooseGfxOption(setting, value) {
        if (setting === 'preset') {
            if (!GFX_PRESETS[value]) return;
            Object.assign(gfx, GFX_PRESETS[value]);
        } else {
            if (!GFX_OPTIONS[setting] || !GFX_OPTIONS[setting].includes(value)) return;
            gfx[setting] = value;
        }
        writeStored('kula_invaders_gfx', JSON.stringify(gfx));
        applyGfx();
    }

    // applyGfx makes the renderer and the settings panel follow gfx.
    function applyGfx() {
        if (stars.length !== STAR_COUNTS[gfx.stars]) initStars();
        if (particles.length > PARTICLE_LIMITS[gfx.particles]) particles.length = PARTICLE_LIMITS[gfx.particles];
        resize();

        const preset = Object.keys(GFX_PRESETS).find(name =>
            Object.keys(GFX_PRESETS[name]).every(k => GFX_PRESETS[name][k] === gfx[k])) || '';
        for (const group of $settingsScreen.querySelectorAll('.settings-options')) {
            const current = group.dataset.setting === 'preset' ? preset : gfx[group.dataset.setting];
            for (const btn of group.querySelectorAll('.opt-btn')) {
                btn.setAttribute('aria-pressed', String(btn.dataset.value === current));
            }
        }
    }

    // Opening the settings pauses a running game; the other overlays are
    // hidden meanwhile so changes preview on the live canvas.
    function openSettings() {
        if (settingsOpen) return;
        if (state === 'playing') togglePause();
        settingsOpen = true;
        document.body.classList.add('settings-open');
        $settingsScreen.classList.remove('hidden');
        const selected = $settingsScreen.querySelector('.opt-btn[aria-pressed="true"]');
        (selected || $settingsScreen.querySelector('.opt-btn')).focus({ preventScroll: true });
    }

    function closeSettings() {
        if (!settingsOpen) return;
        settingsOpen = false;
        document.body.classList.remove('settings-open');
        $settingsScreen.classList.add('hidden');
        releaseFocus();
    }

    // Keyboard play must not reach a HUD or menu button that kept focus after
    // a click, or Space and Enter would press it again.
    function releaseFocus() {
        const el = document.activeElement;
        if (el && el !== document.body && typeof el.blur === 'function') el.blur();
    }

    function toggleMute() {
        isMuted = !isMuted;
        writeStored('kula_invaders_muted', String(isMuted));
        updateMuteIcon();
    }

    function updateMuteIcon() {
        if (!$muteIcon) return;
        if (isMuted) {
            $muteIcon.innerHTML = `<path d="M11 5L6 9H2v6h4l5 4V5zM23 9l-6 6M17 9l6 6" />`;
            $btnMute.style.opacity = '0.5';
        } else {
            $muteIcon.innerHTML = `<path d="M11 5L6 9H2v6h4l5 4V5zM19.07 4.93a10 10 0 0 1 0 14.14M15.54 8.46a5 5 0 0 1 0 7.07" />`;
            $btnMute.style.opacity = '1';
        }
    }

    function toggleFullscreen() {
        if (!document.fullscreenElement) {
            document.documentElement.requestFullscreen().catch(err => {
                console.warn(`Error attempting to enable full-screen mode: ${err.message}`);
            });
        } else {
            if (document.exitFullscreen) {
                document.exitFullscreen();
            }
        }
    }

    function togglePause() {
        if (settingsOpen) return;
        if (state === 'playing') {
            state = 'paused';
            $pauseScreen.classList.remove('hidden');
            canvas.style.cursor = 'default';
        } else if (state === 'paused') {
            state = 'playing';
            $pauseScreen.classList.add('hidden');
            canvas.style.cursor = 'none';
            releaseFocus();
        }
        updateMobileControlsVisibility();
    }

    function updateMobileControlsVisibility() {
        if (!isMobile || !$mobileControls) return;
        if (state === 'playing') {
            $mobileControls.classList.remove('hidden');
        } else {
            $mobileControls.classList.add('hidden');
        }
    }

    // Replace keyboard pause logic to use the shared function
    // (Search for Escape key handling in keyboard listener below)
    
    setupMobileControls();

    // -------------------------------------------------------
    // Init
    // -------------------------------------------------------
    applyGfx();
    updateHUD(true);
    updateMobileControlsVisibility();
    loop(performance.now());

})();
