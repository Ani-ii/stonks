# Cache/latency probe for NSE and BSE announcement feeds (PowerShell, no compiled program needed).
# Paste into a PowerShell window, or run: powershell -ExecutionPolicy Bypass -File .\probe.ps1
# Runs 40 rounds, 3 seconds apart (about 2 minutes). Paste the whole output back.
$ua   = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36'
$nseH = @{ 'User-Agent'=$ua; 'Accept'='application/json, text/plain, */*'; 'Referer'='https://www.nseindia.com/companies-listing/corporate-filings-announcements' }
$bseH = @{ 'User-Agent'=$ua; 'Accept'='application/json, text/plain, */*'; 'Origin'='https://www.bseindia.com/'; 'Referer'='https://www.bseindia.com/'; 'Sec-Fetch-Site'='same-site' }
$nseUrl = 'https://www.nseindia.com/api/corporate-announcements?index=equities'
$d = Get-Date -Format yyyyMMdd
$bseUrl = "https://api.bseindia.com/BseIndiaAPI/api/AnnSubCategoryGetData/w?pageno=1&strCat=-1&subcategory=-1&strPrevDate=$d&strToDate=$d&strSearch=P&strScrip=&strType=C"
$nseDates = @{}; $bseDates = @{}
1..40 | ForEach-Object {
  foreach ($src in 'NSE','BSE') {
    $t = Get-Date
    try {
      if ($src -eq 'NSE') {
        if ($nseS) { $r = Invoke-WebRequest -UseBasicParsing -Uri $nseUrl -Headers $nseH -WebSession $nseS }
        else       { $r = Invoke-WebRequest -UseBasicParsing -Uri $nseUrl -Headers $nseH -SessionVariable nseS }
        $j = $r.Content | ConvertFrom-Json; $newest = $j[0].exchdisstime
      } else {
        $r = Invoke-WebRequest -UseBasicParsing -Uri $bseUrl -Headers $bseH
        $j = $r.Content | ConvertFrom-Json; $newest = $j.Table[0].DissemDT
      }
      $ms = [int]((Get-Date) - $t).TotalMilliseconds
      $dt = $r.Headers['Date']
      if ($src -eq 'NSE') { $nseDates[$dt] = 1 } else { $bseDates[$dt] = 1 }
      "{0} {1} {2,4}ms Date={3} Age={4} Cache-Control={5} newest={6} bytes={7}" -f (Get-Date -Format HH:mm:ss), $src, $ms, $dt, $r.Headers['Age'], $r.Headers['Cache-Control'], $newest, $r.Content.Length
    } catch { "{0} {1} ERROR {2}" -f (Get-Date -Format HH:mm:ss), $src, $_.Exception.Message }
  }
  Start-Sleep -Seconds 3
}
"SUMMARY: 40 polls per source. Distinct Date header values: NSE=$($nseDates.Count) BSE=$($bseDates.Count)"
"If those counts are far below 40, responses are being cached."
