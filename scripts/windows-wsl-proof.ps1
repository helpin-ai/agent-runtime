# Disposable WSL2 proof only. Never imports, repairs or enables Rex's product sandbox.
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true
$root = Join-Path $env:RUNNER_TEMP ('rex-wsl-proof-' + [Guid]::NewGuid().ToString('N'))
$distribution = 'Rex-Confinement-CI-' + [Guid]::NewGuid().ToString('N')
New-Item -ItemType Directory $root | Out-Null
$archive = Join-Path $root rootfs.tar.gz
Invoke-WebRequest https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/x86_64/alpine-minirootfs-3.22.2-x86_64.tar.gz -OutFile $archive
if ((Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne '412454ed98025ad9cd910d13f3d448b184e81502baa83180e89f98d0f13674be') { throw 'WSL proof rootfs checksum mismatch' }
$env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
go test -c -o (Join-Path $root sandbox.test) ./internal/sandbox
[IO.File]::WriteAllText((Join-Path $root 'fake-credential.txt'), 'fixture; never a real account credential')
function LinuxPath([string]$Path) {
    $absolute = [IO.Path]::GetFullPath($Path)
    if ($absolute -notmatch '^[A-Za-z]:\\') { throw 'Proof requires a local Windows drive' }
    return '/mnt/' + $absolute.Substring(0,1).ToLowerInvariant() + $absolute.Substring(2).Replace('\','/')
}
$imported = $false
try {
    wsl.exe --status
    wsl.exe --import $distribution (Join-Path $root distribution) $archive --version 2
    $imported = $true
    wsl.exe -d $distribution --exec /bin/sh -c 'printf "[interop]\nenabled=false\nappendWindowsPath=false\n" > /etc/wsl.conf'
    wsl.exe --terminate $distribution
    $linuxRoot = LinuxPath $root
    wsl.exe -d $distribution --exec /bin/sh -c 'mkdir -p /tmp/rex-proof; cp "$1/sandbox.test" /tmp/rex-proof/sandbox.test; chmod 700 /tmp/rex-proof/sandbox.test' sh $linuxRoot
    wsl.exe -d $distribution --exec ls -la $linuxRoot
    if (!(Test-Path (Join-Path $root 'fake-credential.txt'))) { throw 'Windows fixture was not created' }
    $secret = LinuxPath (Join-Path $root 'fake-credential.txt')
    $windowsExe = LinuxPath (Join-Path $env:SystemRoot System32/cmd.exe)
    # Separate runs prove ext4 and Windows-mounted workspaces independently.
    foreach ($temp in @('/tmp/rex-proof', "$linuxRoot/workspaces")) {
        wsl.exe -d $distribution --exec mkdir -p $temp
        wsl.exe -d $distribution --exec env "TMPDIR=$temp" SANDBOX_WSL_PROOF=1 "SANDBOX_WINDOWS_SECRET=$secret" "SANDBOX_WINDOWS_EXE=$windowsExe" /tmp/rex-proof/sandbox.test '-test.run=^TestWSLConfinementProof$' '-test.v=true'
    }
} finally {
    # Only this unique CI-created distribution can be removed.
    if ($imported) { wsl.exe --unregister $distribution }
}
