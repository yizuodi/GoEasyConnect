const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const cp = require('node:child_process');
const test = require('node:test');

function exercise(options = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'easyconnect-updater-test-'));
  try {
    const runtime = path.join(root, 'runtime');
    fs.mkdirSync(runtime);
    const database = path.join(runtime, 'data.db');
    fs.writeFileSync(database, 'original database');
    fs.writeFileSync(path.join(runtime, 'config.json'), '{}');
    const binary = (version, fail = false) => `#!/bin/bash\ncase "$1" in\n--version) echo ${version};;\n--print-database-path) echo '${database}';;\n--check) ${fail ? `echo changed > '${database}'; exit 1` : 'exit 0'};;\nesac\n`;
    fs.writeFileSync(path.join(runtime, 'easyconnect'), binary('v0.5.1'), {mode:0o755});
    fs.writeFileSync(path.join(root, 'unit'), `User=${os.userInfo().username}\nGroup=${cp.execFileSync('id',['-gn'],{encoding:'utf8'}).trim()}\nExecStart=${runtime}/easyconnect --config ${runtime}/config.json\n`);
    const packageDir = path.join(root, 'package');
    fs.mkdirSync(packageDir);
    fs.writeFileSync(path.join(packageDir,'easyconnect'),binary('v0.5.2',options.healthFailure),{mode:0o755});
    fs.writeFileSync(path.join(packageDir,'update-helper.sh'),'#!/bin/bash\n# legacy helper\n',{mode:0o755});
    const archive = path.join(root,'archive.tar.gz');
    cp.execFileSync('tar',['-czf',archive,'-C',packageDir,'.']);
    const hash = crypto.createHash('sha256').update(fs.readFileSync(archive)).digest('hex');
    const archiveName = 'GoEasyConnect_v0.5.2_linux_amd64.tar.gz';
    const release = {tag_name:'v0.5.2',assets:[{name:archiveName,digest:'sha256:'+(options.digestFailure?'0'.repeat(64):hash)}]};
    fs.writeFileSync(path.join(root,'release.json'),options.invalidJSON?'not JSON':JSON.stringify(release,null,options.pretty?2:0));
    fs.writeFileSync(path.join(root,'checksums.txt'),hash+'  '+archiveName+'\n');
    let source = fs.readFileSync(path.join(__dirname,'update-helper.sh'),'utf8')
      .replace('[[ ${EUID} -eq 0 ]] || die "must run as root"',': # root check bypassed only for sandbox fixture')
      .replace('backup_root="/var/backups/easyconnect"',`backup_root="${root}/backups"`)
      .replace('lock_file="/run/lock/goeasyconnect-updater.lock"',`lock_file="${root}/lock"`)
      .replace('status_dir="/var/lib/goeasyconnect-updater"',`status_dir="${root}/status"`)
      .replaceAll('/usr/local/libexec/goeasyconnect-updater',root+'/helper');
    fs.writeFileSync(path.join(root,'helper'),'# updater-protocol=2\n',{mode:0o755});
    fs.writeFileSync(path.join(root,'script'),source);
    const mocks = `
systemctl() {
  case "$1" in
    show) echo '${root}/unit';;
    is-active) test ! -f '${root}/stopped';;
    stop) touch '${root}/stopped';;
    start) command rm -f '${root}/stopped';;
  esac
}
install() {
  local args=()
  while (($#)); do case "$1" in -o|-g) shift 2;; *) args+=("$1"); shift;; esac; done
  command install "\${args[@]}"
}
cp() {
  ${options.backupFailure ? `[[ "$*" == *'/database/database'* ]] && return 1` : ':'}
  command cp "$@"
}
uname() { echo x86_64; }
runuser() {
  while [[ "$1" != -- ]]; do shift; done; shift
  [[ "$*" != *'/package/easyconnect'* ]] || { echo 'private temporary directory inaccessible' >&2; return 1; }
  "$@"
}
curl() {
  local destination='' url="\${!#}"
  if [[ "$url" == *'/releases/latest' ]]; then command cat '${root}/release.json'; return; fi
  ${options.proxy ? `[[ "$url" != https://ghfast.top/* ]] && return 28` : ':'}
  while (($#)); do if [[ "$1" == -o ]]; then destination="$2"; shift; fi; shift; done
  if [[ "$url" == *checksums.txt ]]; then command cp '${root}/checksums.txt' "$destination"; else command cp '${archive}' "$destination"; fi
}
source '${root}/script'
`;
    const result=cp.spawnSync('bash',['-c',mocks],{encoding:'utf8',timeout:10000});
    const progress=JSON.parse(fs.readFileSync(path.join(root,'status/status.json'),'utf8'));
    assert.equal(result.status,options.invalidJSON||options.digestFailure||options.healthFailure||options.backupFailure?1:0,result.stderr);
    assert.equal(progress.state,result.status===0?'succeeded':'failed');
    assert.equal(fs.readFileSync(database,'utf8'),'original database');
    assert.equal(fs.existsSync(path.join(root,'stopped')),false,'service must not remain stopped');
    const installed=cp.execFileSync(path.join(runtime,'easyconnect'),['--version'],{encoding:'utf8'}).trim();
    assert.equal(installed,result.status===0?'v0.5.2':'v0.5.1');
    assert.equal(fs.readFileSync(path.join(root,'helper'),'utf8'),'# updater-protocol=2\n','legacy package must not overwrite repaired updater');
    if(options.digestFailure)assert.equal(progress.phase,'verify');
  } finally {fs.rmSync(root,{recursive:true,force:true});}
}

test('updater parses compact JSON and keeps private staging accessible only to root',()=>exercise());
test('updater parses formatted JSON',()=>exercise({pretty:true}));
test('updater rejects malformed release JSON without stopping service',()=>exercise({invalidJSON:true}));
test('updater rejects incorrect GitHub asset digest',()=>exercise({digestFailure:true}));
test('updater rolls back binary and database after health check failure',()=>exercise({healthFailure:true}));
test('updater restores service after backup failure',()=>exercise({backupFailure:true}));
test('updater uses verified proxy fallback after direct download failure',()=>exercise({proxy:true}));
