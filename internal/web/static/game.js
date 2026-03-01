/* ============================================================
   Space Invaders – Game Engine
   Vanilla JS, Canvas 2D, Web Audio API
   ============================================================ */

(function () {
    'use strict';

    // -------------------------------------------------------
    // Constants
    // -------------------------------------------------------
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
    const STAR_COUNT = 120;
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
    // Audio (Web Audio API — synthesized)
    // -------------------------------------------------------
    let audioCtx = null;
    function ensureAudio() {
        if (!audioCtx) audioCtx = new (window.AudioContext || window.webkitAudioContext)();
    }

    function playTone(freq, duration, type, vol) {
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

    // -------------------------------------------------------
    // State
    // -------------------------------------------------------
    let state = 'start'; // start | playing | paused | gameover | levelup
    let score = 0;
    let level = 1;
    let lives = 3;
    let highScore = parseInt(localStorage.getItem('kula_invaders_high') || '0', 10);
    let shootCooldown = 0;

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
    let enemyMoveTimer = 0;
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
    function resize() {
        const maxW = window.innerWidth * 0.92;
        const maxH = window.innerHeight * 0.88;
        scale = Math.min(maxW / GAME_W, maxH / GAME_H, 1.2);
        canvas.width = GAME_W;
        canvas.height = GAME_H;
        canvas.style.width = (GAME_W * scale) + 'px';
        canvas.style.height = (GAME_H * scale) + 'px';
    }
    window.addEventListener('resize', resize);
    resize();

    // -------------------------------------------------------
    // Stars
    // -------------------------------------------------------
    function initStars() {
        stars = [];
        for (let i = 0; i < STAR_COUNT; i++) {
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
        for (const s of stars) {
            s.y += s.speed;
            if (s.y > GAME_H) { s.y = 0; s.x = Math.random() * GAME_W; }
            s.brightness = 0.3 + Math.sin(Date.now() * 0.002 + s.x) * 0.2;
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

        if ((keys[' '] || keys['ArrowUp'] || keys['w']) && shootCooldown <= 0) {
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
        ctx.shadowColor = '#3b82f6';
        ctx.shadowBlur = 12;
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
        ctx.shadowColor = '#06b6d4';
        ctx.shadowBlur = 8;
        ctx.fillRect(cx - 4, player.y + player.h, 8, 4 + Math.sin(Date.now() * 0.02) * 2);
        ctx.shadowBlur = 0;

        // Shield bubble
        if (shieldHP > 0) {
            const alpha = 0.15 + shieldHP * 0.08;
            ctx.strokeStyle = `rgba(59, 130, 246, ${alpha})`;
            ctx.shadowColor = '#3b82f6';
            ctx.shadowBlur = 10;
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
        enemyMoveTimer = 0;
        enemyShootTimer = 0;
    }

    function updateEnemies() {
        // Recalculate speed based on how many are alive
        const aliveCount = enemies.filter(e => e.alive).length;
        if (aliveCount === 0) return;
        enemySpeed = 0.4 + level * 0.15 + (1 - aliveCount / (ENEMY_ROWS * ENEMY_COLS)) * 2.5;

        // Move enemies
        let hitEdge = false;
        for (const e of enemies) {
            if (!e.alive) continue;
            e.x += enemyDir * enemySpeed;
            if (e.x + e.w > GAME_W - 5 || e.x < 5) hitEdge = true;
        }

        if (hitEdge) {
            enemyDir *= -1;
            for (const e of enemies) {
                if (!e.alive) continue;
                e.y += ENEMY_STEP_DOWN;
                // Check if enemies reached the player
                if (e.y + e.h >= player.y - 10) {
                    gameOver();
                    return;
                }
            }
        }

        // Enemy shooting
        enemyShootTimer++;
        const shootInterval = Math.max(20, 60 - level * 5);
        if (enemyShootTimer >= shootInterval) {
            enemyShootTimer = 0;
            // Pick a random alive enemy from a bottom row
            const alive = enemies.filter(e => e.alive);
            if (alive.length > 0) {
                // Prefer bottom enemies
                const cols = {};
                for (const e of alive) {
                    const cKey = Math.round(e.x / (ENEMY_W + ENEMY_PAD_X));
                    if (!cols[cKey] || e.y > cols[cKey].y) cols[cKey] = e;
                }
                const bottoms = Object.values(cols);
                const shooter = bottoms[Math.floor(Math.random() * bottoms.length)];
                enemyBullets.push({
                    x: shooter.x + shooter.w / 2,
                    y: shooter.y + shooter.h,
                    color: ROW_COLORS[shooter.row] || '#ef4444',
                });
            }
        }

        // Animate frames
        enemyMoveTimer++;
        if (enemyMoveTimer % 30 === 0) {
            for (const e of enemies) e.frame = 1 - e.frame;
        }
    }

    function drawEnemies() {
        for (const e of enemies) {
            if (!e.alive) continue;
            const color = ROW_COLORS[e.row] || '#ef4444';
            ctx.fillStyle = color;
            ctx.shadowColor = color;
            ctx.shadowBlur = 6;

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
        ctx.shadowColor = '#06b6d4';
        ctx.shadowBlur = 8;
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
            ctx.shadowColor = b.color || '#ef4444';
            ctx.shadowBlur = 6;
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
            ctx.shadowColor = p.color;
            ctx.shadowBlur = 4;
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
            ctx.shadowColor = info.color;
            ctx.shadowBlur = 12;
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
    function gameOver() {
        state = 'gameover';
        if (score > highScore) {
            highScore = score;
            localStorage.setItem('kula_invaders_high', String(highScore));
        }
        $finalScore.textContent = score;
        $finalHigh.textContent = highScore;
        $gameoverScreen.classList.remove('hidden');
        SFX.explode();
        canvas.style.cursor = 'default';
    }

    function nextLevel() {
        level++;
        state = 'levelup';
        $levelupNum.textContent = level;
        $levelupScreen.classList.remove('hidden');
        SFX.levelUp();
        canvas.style.cursor = 'default';
        setTimeout(() => {
            $levelupScreen.classList.add('hidden');
            initEnemies();
            playerBullets = [];
            enemyBullets = [];
            powerups = [];
            state = 'playing';
            canvas.style.cursor = 'none';
        }, 2000);
    }

    function startGame() {
        ensureAudio();
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
        updateHUD();
        $startScreen.classList.add('hidden');
        $gameoverScreen.classList.add('hidden');
        $pauseScreen.classList.add('hidden');
        state = 'playing';
        canvas.style.cursor = 'none';
    }

    // -------------------------------------------------------
    // HUD
    // -------------------------------------------------------
    function updateHUD() {
        $hudScore.textContent = score;
        $hudLevel.textContent = level;
        $hudHigh.textContent = highScore;
        $hudLives.textContent = '♥'.repeat(Math.max(0, lives));
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

        // Check level complete
        if (enemies.filter(e => e.alive).length === 0) {
            nextLevel();
        }
    }

    function draw() {
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
    }

    function loop() {
        update();
        draw();
        requestAnimationFrame(loop);
    }

    // -------------------------------------------------------
    // Input
    // -------------------------------------------------------
    window.addEventListener('keydown', (e) => {
        keys[e.key] = true;

        if (e.key === 'Enter') {
            if (state === 'start' || state === 'gameover') {
                startGame();
            }
        }

        if (e.key === 'Escape') {
            if (state === 'playing') {
                state = 'paused';
                $pauseScreen.classList.remove('hidden');
                canvas.style.cursor = 'default';
            } else if (state === 'paused') {
                state = 'playing';
                $pauseScreen.classList.add('hidden');
                canvas.style.cursor = 'none';
            }
        }

        // Prevent scrolling with space/arrows
        if (['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', ' '].includes(e.key)) {
            e.preventDefault();
        }
    });

    window.addEventListener('keyup', (e) => {
        keys[e.key] = false;
    });

    // -------------------------------------------------------
    // Init
    // -------------------------------------------------------
    initStars();
    $hudHigh.textContent = highScore;
    loop();

})();
