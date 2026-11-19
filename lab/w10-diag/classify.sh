#!/bin/bash
cd ~/lab-sw-out/w10-diag/rate3
for f in r-*.txt; do
  l=$(grep -h "case 865: goroutine" $f) || continue
  op=$(echo "$l" | grep -oE '(Touch|Count): Upper [0-9]+ \(the overflow table.s [0-9]+\) > [0-9]+')
  ck=$(echo "$l" | grep -oE 'after [0-9]+ \(now \+[0-9-]+\)' | grep -oE '\+[0-9-]+')
  reads=$(echo "$l" | grep -oE '\{now [0-9]+ epoch [0-9]+ until [0-9]+ -> Upper [0-9]+ Count [0-9]+' | awk '{printf "e%s:U%s ", $4, $9}')
  echo "$f | $op | clock after now$ck | $reads"
done > classify.txt
echo "failures: $(wc -l < classify.txt)"
# reads: before, call..., after, whereIs. Same epoch at before, at the call's nonzero read and at after?
awk -F'|' '{n=split($4,a," "); split(a[1],b,":"); split(a[n-1],c,":"); call=""; for(i=2;i<n-1;i++){split(a[i],d,":"); if(d[2]!="U0") call=d[1]} 
  if (call=="") k="call read 0?"; else if (b[1]==call && call==c[1]) k="same epoch at all three"; else if (call==c[1]) k="epoch moved before the call only"; else k="epoch moved between the call and the after-read"; print k}' classify.txt | sort | uniq -c
awk -F'|' '{print $3}' classify.txt | tr -d ' ' | sort -t+ -k2 -n | tr '\n' ' '; echo
