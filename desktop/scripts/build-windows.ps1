[CmdletBinding()]
param([switch]$Launch)

$ErrorActionPreference = 'Stop'
if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    throw 'Build the Windows package on a Windows host.'
}

$desktopDir = Split-Path -Parent $PSScriptRoot
$outputDir = Join-Path (Split-Path -Parent $desktopDir) 'dist'
$null = New-Item -ItemType Directory -Force -Path $outputDir

$binary = Join-Path $outputDir 'slopguard-desktop.exe'
$archive = Join-Path $outputDir 'slopguard-windows.zip'

Push-Location $desktopDir
try {
    & go build -trimpath '-ldflags=-H=windowsgui' -o $binary .
    if ($LASTEXITCODE -ne 0) {
        throw "go build exited $LASTEXITCODE"
    }

    $file = [IO.File]::OpenRead($binary)
    try {
        $reader = [IO.BinaryReader]::new($file)
        if ($reader.ReadUInt16() -ne 0x5a4d) {
            throw 'Invalid Windows executable header.'
        }
        $file.Position = 0x3c
        $peOffset = $reader.ReadUInt32()
        $file.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x4550) {
            throw 'Invalid PE signature.'
        }
        $file.Position = $peOffset + 24 + 68
        if ($reader.ReadUInt16() -ne 2) {
            throw 'Expected the Windows GUI subsystem.'
        }
    } finally {
        $file.Dispose()
    }

    Compress-Archive -LiteralPath $binary -DestinationPath $archive -Force
    Write-Output "Windows GUI executable: $binary"
    Write-Output "Windows archive: $archive"
    if ($Launch) {
        Start-Process -FilePath $binary
    }
} finally {
    Pop-Location
}
