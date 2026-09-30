# assemble.ps1 - THE SAME WIND / photo-roman assembly
# 6 generated clips (5.13s each) -> 119.4s picture master.
# Segment structure: hold on first frame 4s -> slowed live motion 7.9s -> hold again 4s.
# 0.2s black breath between segments.
# ffmpeg 3.0.1 has no xfade / tpad / loop, so holds come from PNG + -loop 1 and joins use concat.

$ErrorActionPreference = "Stop"
$OutputEncoding = [System.Text.Encoding]::UTF8

$RunDir = "D:\ai_project\ai-movie-studio\runs\run-20260926-093000-fenzhang"
$OutDir = "D:\ai_project\ai-movie-studio\projects\same-wind\film"
$Ffmpeg = "C:\Users\Administrator\AppData\Roaming\bilibili\ffmpeg\ffmpeg.exe"

$W = 1920; $H = 1080; $FPS = 30
# Target 19.9s per segment so the six segments sum to 119.4s, matching the narration timeline
# that studio deliver produced (lines at 0.28 / 20.08 / 39.98 / 59.88 / 79.78 / 99.68).
# 6.0 + 7.9 + 6.0 = 19.9
$HOLD_A = 6.0; $MOTION = 7.9; $HOLD_B = 6.0
$SHOTS = @("sw001","sw002","sw003","sw004","sw005","sw006")

$segDir = Join-Path $OutDir "_segments"
New-Item -ItemType Directory -Force -Path $segDir | Out-Null
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

# CRF 18 for intermediates: visually lossless enough, and keeps us from re-encoding twice at CRF 16.
$CRF = 18
$scalePad = "scale=${W}:${H}:force_original_aspect_ratio=decrease,pad=${W}:${H}:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=${FPS},format=yuv420p"
$setptsFactor = [math]::Round($MOTION / 5.13, 3)

Write-Host ("setpts factor = {0}  (5.13s -> {1}s, target {2}s per segment)" -f $setptsFactor, $MOTION, ($HOLD_A + $MOTION + $HOLD_B))

function Run-Ffmpeg {
    param([string]$Label, [string[]]$FfArgs)
    & $Ffmpeg -hide_banner -loglevel error -y @FfArgs
    if ($LASTEXITCODE -ne 0) { throw ("ffmpeg failed [{0}] exit={1}" -f $Label, $LASTEXITCODE) }
}

foreach ($id in $SHOTS) {
    $png = Join-Path $RunDir "output\shots\$id.png"
    $mp4 = Join-Path $RunDir "output\shots\$id.mp4"
    if (-not (Test-Path $png)) { throw "missing first frame: $png" }
    if (-not (Test-Path $mp4)) { throw "missing clip: $mp4" }

    $holdA = Join-Path $segDir ($id + "_holdA.mp4")
    $slow  = Join-Path $segDir ($id + "_slow.mp4")
    $seg   = Join-Path $segDir ($id + "_seg.mp4")

    Run-Ffmpeg -Label "$id hold" -FfArgs @(
        "-loop","1","-framerate","$FPS","-i",$png,
        "-t","$HOLD_A",
        "-vf",$scalePad,
        "-c:v","libx264","-preset","medium","-crf","$CRF",
        $holdA
    )

    Run-Ffmpeg -Label "$id slow" -FfArgs @(
        "-i",$mp4,
        "-vf","setpts=${setptsFactor}*PTS,$scalePad",
        "-an",
        "-c:v","libx264","-preset","medium","-crf","$CRF",
        $slow
    )

    # concat demuxer requires ONE "file" directive PER LINE. Writing them space-separated
    # silently drops everything but the first entry (which cost us a 4s segment instead of 19.9s).
    $listFile = Join-Path $segDir ($id + "_list.txt")
    $lines = @(
        ("file '" + $holdA.Replace('\','/') + "'"),
        ("file '" + $slow.Replace('\','/')  + "'"),
        ("file '" + $holdA.Replace('\','/') + "'")
    )
    [System.IO.File]::WriteAllText($listFile, ($lines -join "`r`n") + "`r`n", (New-Object System.Text.ASCIIEncoding))

    Run-Ffmpeg -Label "$id concat" -FfArgs @(
        "-f","concat","-safe","0","-i",$listFile,
        "-c","copy",
        $seg
    )

    Write-Host ("  {0}  segment ok" -f $id)
}

# 0.2s of black between segments, so the film breathes instead of jump-cutting.
$black = Join-Path $segDir "_black.mp4"
Run-Ffmpeg -Label "black" -FfArgs @(
    "-f","lavfi","-i","color=c=black:s=${W}x${H}:d=0.2:r=${FPS}",
    "-c:v","libx264","-preset","medium","-crf","$CRF","-pix_fmt","yuv420p",
    $black
)

# No black slugs between segments: the narration timeline from studio deliver places lines at
# 0.28 / 20.08 / 39.98 / 59.88 / 79.78 / 99.68, i.e. exactly every 19.9s. Inserting 0.2s gaps
# would push the picture 1.0s behind the voice by the final segment. Each segment already opens
# and closes on a held still, which reads as a full stop on its own.
$fullList = Join-Path $segDir "_full_list.txt"
$entries = New-Object System.Collections.ArrayList
for ($i = 0; $i -lt $SHOTS.Count; $i++) {
    [void]$entries.Add("file '" + (Join-Path $segDir ($SHOTS[$i] + "_seg.mp4")).Replace('\','/') + "'")
}
[System.IO.File]::WriteAllText($fullList, ($entries -join "`r`n") + "`r`n", (New-Object System.Text.ASCIIEncoding))

$master = Join-Path $OutDir "picture_master.mp4"
Run-Ffmpeg -Label "master" -FfArgs @(
    "-f","concat","-safe","0","-i",$fullList,
    "-vf","fade=t=in:st=0:d=1.2,fade=t=out:st=118.2:d=1.2",
    "-c:v","libx264","-preset","medium","-crf","$CRF","-pix_fmt","yuv420p",
    $master
)

Write-Host ""
Write-Host ("picture master: {0}" -f $master)
Write-Host ("  size = {0:N1} MB" -f ((Get-Item $master).Length / 1MB))
Write-Host "  (打印时长请用 ffmpeg -i 单独查看；脚本内探测会因退出码 1 中断)"
